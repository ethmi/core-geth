// Copyright 2019 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package rawdb

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math/big"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/metrics"
	"github.com/ethereum/go-ethereum/rlp"
)

type freezerRemoteS3 struct {
	session *session.Session
	service *s3.S3

	namespace string
	quit      chan struct{}
	mu        sync.Mutex

	readMeter  metrics.Meter // Meter for measuring the effective amount of data read
	writeMeter metrics.Meter // Meter for measuring the effective amount of data written
	sizeGauge  metrics.Gauge // Gauge for tracking the combined size of all freezer tables

	uploader   *s3manager.Uploader
	downloader *s3manager.Downloader

	frozen          *uint64 // the length of the frozen blocks (next appended must == val)
	objectGroupSize uint64  // how many blocks to include in a single S3 object

	retrieved map[uint64]AncientObjectS3
	cache     []AncientObjectS3

	log log.Logger
}

type AncientObjectS3 struct {
	Hash       common.Hash                `json:"hash"`
	Header     *types.Header              `json:"header"`
	Body       *types.Body                `json:"body"`
	Receipts   []*types.ReceiptForStorage `json:"receipts"`
	Difficulty *big.Int                   `json:"difficulty"`
}

func NewAncientObjectS3(hashB, headerB, bodyB, receiptsB, difficultyB []byte) (*AncientObjectS3, error) {
	var err error

	hash := common.BytesToHash(hashB)

	header := &types.Header{}
	err = rlp.DecodeBytes(headerB, header)
	if err != nil {
		return nil, err
	}
	body := &types.Body{}
	err = rlp.DecodeBytes(bodyB, body)
	if err != nil {
		return nil, err
	}
	receipts := []*types.ReceiptForStorage{}
	err = rlp.DecodeBytes(receiptsB, &receipts)
	if err != nil {
		return nil, err
	}
	difficulty := new(big.Int)
	err = rlp.DecodeBytes(difficultyB, difficulty)
	if err != nil {
		return nil, err
	}
	return &AncientObjectS3{
		Hash:       hash,
		Header:     header,
		Body:       body,
		Receipts:   receipts,
		Difficulty: difficulty,
	}, nil
}

func (o *AncientObjectS3) RLPBytesForKind(kind string) []byte {
	switch kind {
	case freezerHashTable:
		return o.Hash.Bytes()
	case freezerHeaderTable:
		b, err := rlp.EncodeToBytes(o.Header)
		if err != nil {
			log.Crit("Failed to RLP encode block header", "err", err)
		}
		return b
	case freezerBodiesTable:
		b, err := rlp.EncodeToBytes(o.Body)
		if err != nil {
			log.Crit("Failed to RLP encode block body", "err", err)
		}
		return b
	case freezerReceiptTable:
		b, err := rlp.EncodeToBytes(o.Receipts)
		if err != nil {
			log.Crit("Failed to RLP encode block receipts", "err", err)
		}
		return b
	case freezerDifficultyTable:
		b, err := rlp.EncodeToBytes(o.Difficulty)
		if err != nil {
			log.Crit("Failed to RLP encode block difficulty", "err", err)
		}
		return b
	default:
		panic(fmt.Sprintf("unknown kind: %s", kind))
	}
}

func awsKeyBlock(number uint64) string {
	// Keep blocks in a dir.
	// This namespaces the resource, separating it from the 'index-marker' object.
	return fmt.Sprintf("blocks/%09d.json", number)
}

func (f *freezerRemoteS3) objectKeyForN(n uint64) string {
	return awsKeyBlock(n / f.objectGroupSize)
}

// TODO: this is superfluous now; bucket names must be user-configured
func (f *freezerRemoteS3) bucketName() string {
	return fmt.Sprintf("%s", f.namespace)
}

func (f *freezerRemoteS3) initializeBucket() error {
	bucketName := f.bucketName()
	start := time.Now()
	f.log.Info("Creating bucket if not exists", "name", bucketName)
	result, err := f.service.CreateBucket(&s3.CreateBucketInput{
		Bucket: aws.String(f.bucketName()),
	})
	if err != nil {
		if aerr, ok := err.(awserr.Error); ok {
			switch aerr.Code() {
			case s3.ErrCodeBucketAlreadyExists, s3.ErrCodeBucketAlreadyOwnedByYou:
				f.log.Debug("Bucket exists", "name", bucketName)
				return nil
			}
		}
		return err
	}
	err = f.service.WaitUntilBucketExists(&s3.HeadBucketInput{
		Bucket: aws.String(f.bucketName()),
	})
	if err != nil {
		return err
	}
	f.log.Info("Bucket created", "name", bucketName, "result", result.String(), "elapsed", time.Since(start))
	return nil
}

func (f *freezerRemoteS3) initCache(n uint64) error {
	f.log.Info("Initializing cache", "n", n)
	key := f.objectKeyForN(n)
	buf := aws.NewWriteAtBuffer([]byte{})
	_, err := f.downloader.Download(buf, &s3.GetObjectInput{
		Bucket: aws.String(f.bucketName()),
		Key:    aws.String(key),
	})
	if err != nil {
		if aerr, ok := err.(awserr.Error); ok {
			switch aerr.Code() {
			case s3.ErrCodeNoSuchKey:
				return errOutOfBounds
			}
		}
		f.log.Error("Download error", "method", "initCache", "error", err, "key", key)
		return err
	}
	err = json.Unmarshal(buf.Bytes(), &f.cache)
	if err != nil {
		return err
	}
	f.log.Info("Finished initializing cache")
	return nil
}

// newFreezer creates a chain freezer that moves ancient chain data into
// append-only flat file containers.
func newFreezerRemoteS3(namespace string, readMeter, writeMeter metrics.Meter, sizeGauge metrics.Gauge) (*freezerRemoteS3, error) {
	var err error

	freezerGroups := uint64(32)
	if v := os.Getenv("GETH_FREEZER_S3_GROUP_OBJECTS"); v != "" {
		i, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			return nil, err
		}
		freezerGroups = i
	}
	f := &freezerRemoteS3{
		namespace:       namespace,
		quit:            make(chan struct{}),
		readMeter:       readMeter,
		writeMeter:      writeMeter,
		sizeGauge:       sizeGauge,
		objectGroupSize: freezerGroups,
		retrieved:       make(map[uint64]AncientObjectS3),
		cache:           []AncientObjectS3{},
		log:             log.New("remote", "s3"),
	}

	/*
		By default NewSession will only load credentials from the shared credentials file (~/.aws/credentials).
		If the AWS_SDK_LOAD_CONFIG environment variable is set to a truthy value the Session will be created from the
		configuration values from the shared config (~/.aws/config) and shared credentials (~/.aws/credentials) files.
		Using the NewSessionWithOptions with SharedConfigState set to SharedConfigEnable will create the session as if the
		AWS_SDK_LOAD_CONFIG environment variable was set.
		> https://docs.aws.amazon.com/sdk-for-go/api/aws/session/
	*/
	f.session, err = session.NewSession()
	if err != nil {
		f.log.Info("Session", "err", err)
		return nil, err
	}
	f.log.Info("New session", "region", f.session.Config.Region)
	f.service = s3.New(f.session)

	// Create buckets per the schema, where each bucket is prefixed with the namespace
	// and suffixed with the schema Kind.
	err = f.initializeBucket()
	if err != nil {
		return f, err
	}

	f.uploader = s3manager.NewUploader(f.session)
	f.uploader.Concurrency = 10

	f.downloader = s3manager.NewDownloader(f.session)

	n, _ := f.Ancients()
	f.frozen = &n

	if n > 0 {
		err = f.initCache(n)
		if err != nil {
			return f, err
		}
	}

	return f, nil
}

// Close terminates the chain freezer, unmapping all the data files.
func (f *freezerRemoteS3) Close() error {
	f.quit <- struct{}{}
	// I don't see any Close, Stop, or Quit methods for the AWS service.
	return nil
}

// HasAncient returns an indicator whether the specified ancient data exists
// in the freezer.
func (f *freezerRemoteS3) HasAncient(kind string, number uint64) (bool, error) {
	v, err := f.Ancient(kind, number)
	if err != nil {
		return false, err
	}

	return v != nil, nil
}

// Ancient retrieves an ancient binary blob from the append-only immutable files.
func (f *freezerRemoteS3) Ancient(kind string, number uint64) ([]byte, error) {
	if atomic.LoadUint64(f.frozen) <= number {
		return nil, nil
	}
	backlogLen := uint64(len(f.cache))
	if remoteHeight := atomic.LoadUint64(f.frozen) - backlogLen; remoteHeight <= number {
		// Take from backlog
		backlogIndex := number - remoteHeight
		o := &f.cache[backlogIndex]
		return o.RLPBytesForKind(kind), nil
	}
	if v, ok := f.retrieved[number]; ok {
		return v.RLPBytesForKind(kind), nil
	}

	// Take from remote
	key := f.objectKeyForN(number)
	buf := aws.NewWriteAtBuffer([]byte{})
	_, err := f.downloader.Download(buf, &s3.GetObjectInput{
		Bucket: aws.String(f.bucketName()),
		Key:    aws.String(key),
	})
	if err != nil {
		if aerr, ok := err.(awserr.Error); ok {
			switch aerr.Code() {
			case s3.ErrCodeNoSuchKey:
				return nil, errOutOfBounds
			}
		}
		f.log.Error("Download error", "method", "Ancient", "error", err, "kind", kind, "key", key, "number", number)
		return nil, err
	}
	target := []AncientObjectS3{}
	err = json.Unmarshal(buf.Bytes(), &target)
	if err != nil {
		return nil, err
	}
	f.retrieved = map[uint64]AncientObjectS3{}
	start := number - (number % f.objectGroupSize)
	for i, v := range target {
		f.retrieved[start+uint64(i)] = v
	}
	i := number%f.objectGroupSize
	if i > uint64(len(target)) - 1 {
		return nil, errOutOfBounds
	}
	return target[i].RLPBytesForKind(kind), nil
}

// Ancients returns the length of the frozen items.
func (f *freezerRemoteS3) Ancients() (uint64, error) {
	if f.frozen != nil {
		return atomic.LoadUint64(f.frozen), nil
	}
	f.log.Info("Retrieving ancients number")
	result, err := f.service.GetObject(&s3.GetObjectInput{
		Bucket: aws.String(f.bucketName()),
		Key:    aws.String("index-marker"),
	})
	if err != nil {
		if aerr, ok := err.(awserr.Error); ok {
			switch aerr.Code() {
			case s3.ErrCodeNoSuchKey:
				return 0, nil
			}
		}
		f.log.Error("GetObject error", "method", "Ancients", "error", err)
		return 0, err
	}
	contents, err := ioutil.ReadAll(result.Body)
	if err != nil {
		return 0, err
	}
	i, err := strconv.ParseUint(string(contents), 10, 64)
	f.log.Info("Finished retrieving ancients num", "n", i)
	return i, err
}

// AncientSize returns the ancient size of the specified category.
func (f *freezerRemoteS3) AncientSize(kind string) (uint64, error) {
	// AWS Go-SDK doesn't support this in a convenient way.
	// This would require listing all objects in the bucket and summing their sizes.
	// This method is only used in the InspectDatabase function, which isn't that
	// important.
	return 0, errNotSupported
}

func (f *freezerRemoteS3) setIndexMarker(number uint64) error {
	f.log.Info("Setting index marker", "number", number)
	numberStr := strconv.FormatUint(number, 10)
	reader := bytes.NewReader([]byte(numberStr))
	_, err := f.service.PutObject(&s3.PutObjectInput{
		Bucket: aws.String(f.bucketName()),
		Key:    aws.String("index-marker"),
		Body:   reader,
	})
	return err
}

// AppendAncient injects all binary blobs belong to block at the end of the
// append-only immutable table files.
//
// Notably, this function is lock free but kind of thread-safe. All out-of-order
// injection will be rejected. But if two injections with same number happen at
// the same time, we can get into the trouble.
func (f *freezerRemoteS3) AppendAncient(number uint64, hash, header, body, receipts, td []byte) (err error) {

	f.mu.Lock()
	defer f.mu.Unlock()

	o, err := NewAncientObjectS3(hash, header, body, receipts, td)
	if err != nil {
		return err
	}

	f.cache = append(f.cache, *o)

	atomic.AddUint64(f.frozen, 1)

	return nil
}

// Truncate discards any recent data above the provided threshold number.
// TODO@meowsbits: handle pagination.
//   ListObjects will only return the first 1000. Need to implement pagination.
//   Also make sure that the Marker is working as expected.
func (f *freezerRemoteS3) TruncateAncients(items uint64) error {

	f.mu.Lock()
	defer f.mu.Unlock()

	n := atomic.LoadUint64(f.frozen)

	// Case where truncation only effects backlogs
	backlogLen := uint64(len(f.cache))
	if n-backlogLen <= items {
		index := items - (n - backlogLen)
		f.cache = f.cache[:index]
		atomic.StoreUint64(f.frozen, items)
		return nil
	}

	// Case where truncate depth is below backlog
	//
	// First, download the latest group object into cache.
	key := f.objectKeyForN(items - 1)
	buf := aws.NewWriteAtBuffer([]byte{})
	_, err := f.downloader.Download(buf, &s3.GetObjectInput{
		Bucket: aws.String(f.bucketName()),
		Key:    aws.String(key),
	})
	if err != nil {
		if aerr, ok := err.(awserr.Error); ok {
			switch aerr.Code() {
			case s3.ErrCodeNoSuchKey:
				return errOutOfBounds
			}
		}
		f.log.Error("Download error", "method", "TruncateAncients", "error", err, "key", key, "items", items)
		return err
	}
	err = json.Unmarshal(buf.Bytes(), &f.cache)
	if err != nil {
		return err
	}
	// Truncating the cache to the remainder number of items
	f.cache = f.cache[:(items % f.objectGroupSize)]

	// Now truncate all remote data from the latest grouped object and beyond.
	// Noting that this can remove data from the remote that is actually antecedent to the
	// desired truncation level, since we have to use the groups.
	// That's why we pulled the object into the cache first; on the next Sync, the un-truncated
	// blocks will be pushed back up to the remote.
	f.log.Info("Truncating ancients", "ancients", n, "target", items, "delta", n-items)
	start := time.Now()

	list := &s3.ListObjectsInput{
		Bucket: aws.String(f.bucketName()),
		Marker: aws.String(f.objectKeyForN(items)),
	}
	iter := s3manager.NewDeleteListIterator(f.service, list)
	batcher := s3manager.NewBatchDeleteWithClient(f.service)
	if err := batcher.Delete(aws.BackgroundContext(), iter); err != nil {
		return err
	}

	err = f.setIndexMarker(items)
	if err != nil {
		return err
	}
	atomic.StoreUint64(f.frozen, items)
	f.log.Info("Finished truncating ancients", "elapsed", time.Since(start))
	return nil
}

// sync flushes all data tables to disk.
func (f *freezerRemoteS3) Sync() error {
	lenBacklog := len(f.cache)
	if lenBacklog == 0 {
		return nil
	}

	var err error

	f.log.Info("Syncing ancients", "backlog.blocks", lenBacklog)
	start := time.Now()

	lenCache := len(f.cache)
	cacheStartN := atomic.LoadUint64(f.frozen) - uint64(lenCache)

	set := []AncientObjectS3{}
	uploads := []s3manager.BatchUploadObject{}
	for i, v := range f.cache {

		set = append(set, v)

		// finalize upload object if we have the group-by number in the set, or if the item is the last
		if uint64(len(set)) == f.objectGroupSize || i == lenCache-1 {
			// seal upload object
			b, err := json.Marshal(set)
			if err != nil {
				return err
			}
			set = []AncientObjectS3{}
			uploads = append(uploads, s3manager.BatchUploadObject{
				Object: &s3manager.UploadInput{
					Bucket: aws.String(f.bucketName()),
					Key:    aws.String(f.objectKeyForN(cacheStartN + uint64(i))),
					Body:   bytes.NewReader(b),
				},
			})
		}
	}

	iter := &s3manager.UploadObjectsIterator{Objects: uploads}
	err = f.uploader.UploadWithIterator(aws.BackgroundContext(), iter)
	if err != nil {
		return err
	}
	rem := uint64(len(f.cache)) % f.objectGroupSize
	// splice first n groups, leaving mod leftovers
	f.cache = f.cache[uint64(len(f.cache))-rem:]

	elapsed := time.Since(start)
	blocksPerSecond := fmt.Sprintf("%0.2f", float64(lenBacklog)/elapsed.Seconds())

	err = f.setIndexMarker(atomic.LoadUint64(f.frozen))
	if err != nil {
		return err
	}

	f.log.Info("Finished syncing ancients", "backlog", lenBacklog, "elapsed", elapsed, "bps", blocksPerSecond)
	return err
}

// repair truncates all data tables to the same length.
func (f *freezerRemoteS3) repair() error {
	/*min := uint64(math.MaxUint64)
	for _, table := range f.tables {
		items := atomic.LoadUint64(&table.items)
		if min > items {
			min = items
		}
	}
	for _, table := range f.tables {
		if err := table.truncate(min); err != nil {
			return err
		}
	}
	atomic.StoreUint64(&f.frozen, min)
	*/
	return nil
}
