// drive_sync syncs Google Drive metadata to a local LevelDB database and provides methods to query it.

package drive_db

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path"
	"sync"
	"time"

	gdrive "code.google.com/p/google-api-go-client/drive/v2"
	"github.com/asjoyner/fuse_gdrive/lru"
	"github.com/golang/groupcache/singleflight"
	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/errors"
	"github.com/syndtr/goleveldb/leveldb/filter"
	"github.com/syndtr/goleveldb/leveldb/opt"
	"github.com/syndtr/goleveldb/leveldb/util"
)

const (
	downloadUrlLifetime = time.Duration(time.Hour * 12)
	// https://developers.google.com/drive/web/folder
	driveFolderMimeType string = "application/vnd.google-apps.folder"
	checkpointVersion          = 2
	dbDataChunkSize            = 256 * 1024 // bytes
)

var (
	debugDriveDB    = flag.Bool("drivedb.debug", false, "print debug statements from the drive_db package and debug enable HTTP handlers which can leak all your data via HTTP.")
	logChanges      = flag.Bool("drivedb.logchanges", false, "Log json encoded metadata as it is fetched from Google Drive.")
	driveDataChunks = flag.Int64("drivedb.fetchsize", 8*1024*1024/dbDataChunkSize, fmt.Sprintf("Chunks of %v bytes to read from drive at a time (readahead).", dbDataChunkSize))
	cacheSize       = flag.Int64("drivedb.maxcachesize", 1024*8, fmt.Sprintf("Chunks of %v bytes to cache from drive at a time.", dbDataChunkSize))
)

type debugging bool

var debug debugging

func (d debugging) Printf(format string, args ...interface{}) {
	if d {
		log.Printf(format, args...)
	}
}

// encode returns the item encoded into []byte.
func encode(item interface{}) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	err := enc.Encode(item)
	return buf.Bytes(), err
}

// decode decodes the data buffer into the item.
func decode(data []byte, item interface{}) error {
	dec := json.NewDecoder(bytes.NewBuffer(data))
	return dec.Decode(item)
}

type File struct {
	*gdrive.File
	Inode                 uint64
	Children              []uint64 // inodes of children
	cachedDownloadUrl     string
	cachedDownloadUrlTime time.Time
}

type CheckPoint struct {
	LastChangeID int64
	LastInode    uint64
	Version      int
	CacheBlock   int64
}

type DownloadSpec struct {
	fileId   string
	chunk    int64
	filesize int64
}

type DriveDB struct {
	sync.Mutex
	client       *http.Client
	service      *gdrive.Service
	db           *leveldb.DB
	data         string // root of cache directory
	syncmu       sync.Mutex
	synced       *sync.Cond
	iters        sync.WaitGroup
	cpt          CheckPoint
	changes      chan *gdrive.ChangeList
	lruCache     *lru.Cache // in-memory inode to *File cache
	pollInterval time.Duration
	sf           singleflight.Group
	dbpath       string
	rootId       string
	driveSize    int64
	pfetchq      chan DownloadSpec
}

func openLevelDB(filepath string) (*leveldb.DB, error) {
	o := &opt.Options{
		Filter: filter.NewBloomFilter(10),
		Strict: opt.StrictAll,
	}
	db, err := leveldb.OpenFile(filepath, o)
	if err == nil {
		return db, nil
	}
	if _, ok := err.(*errors.ErrCorrupted); ok {
		log.Printf("recovering leveldb: %v", err)
		db, err = leveldb.RecoverFile(filepath, o)
		if err != nil {
			log.Printf("failed to recover leveldb: %v", err)
			return nil, err
		}
		return db, nil
	}
	log.Printf("failed to open leveldb: %v", err)
	return nil, err
}

// NewDriveDB creates a new DriveDB and starts syncing metadata.
func NewDriveDB(client *http.Client, filepath string, pollInterval time.Duration, rootId string) (*DriveDB, error) {
	svc, _ := gdrive.New(client)
	_, err := svc.About.Get().Do()
	if err != nil {
		log.Fatalf("drive.service.About.Get().Do: %v\n", err)
	}

	if *debugDriveDB {
		debug = true
	}

	db, err := openLevelDB(path.Join(filepath, "meta"))
	if err != nil {
		return nil, err
	}

	d := &DriveDB{
		client:       client,
		service:      svc,
		db:           db,
		data:         path.Join(filepath, "data"),
		lruCache:     lru.New(int(1000)), // make the value tunable
		changes:      make(chan *gdrive.ChangeList, 200),
		pollInterval: pollInterval,
		dbpath:       filepath,
		rootId:       rootId,
		driveSize:    dbDataChunkSize * (*driveDataChunks), // ensure drive reads are always a multiple of cache size
		pfetchq:      make(chan DownloadSpec, 20000),
	}

	// Get saved checkpoint.
	err = d.get(internalKey("checkpoint"), &d.cpt)
	if err != nil {
		log.Printf("error reading checkpoint: %v", err)
		d.cpt = NewCheckpoint()
	}
	if d.cpt.Version < checkpointVersion {
		log.Printf("checkpoint version invalid, require %v but found %v", checkpointVersion, d.cpt.Version)
		err = d.reinit()
		if err != nil {
			log.Printf("Failed to reinitialize the database: %v", err)
			log.Fatal("You should probably run: rm -rf %v", filepath)
		}
	}
	err = d.writeCheckpoint(nil)
	if err != nil {
		return nil, fmt.Errorf("could not write checkpoint: %v", err)
	}
	debug.Printf("Recovered from checkpoint: %+v", d.cpt)

	if err := d.createRoot(); err != nil {
		return nil, fmt.Errorf("could not create root inode entry: %v", err)
	}

	d.synced = sync.NewCond(&d.syncmu)

	go d.sync()
	go d.pollForChanges()
	if debug {
		registerDebugHandles(*d) // in http_handlers.go
	}
	go d.prefetcher()
	return d, nil
}

func (d *DriveDB) Service() *gdrive.Service {
	return d.service
}

func NewCheckpoint() CheckPoint {
	return CheckPoint{
		LastInode:  1000, // start high, to allow "special" inodes
		Version:    checkpointVersion,
		CacheBlock: 0,
	}
}

// createRoot synthesizes the root of the filesystem, based on the
// rootId provided at instantiation time.
func (d *DriveDB) createRoot() error {
	launch, _ := time.Unix(1335225600, 0).MarshalText()
	file := &gdrive.File{
		Id:                 d.rootId,
		Title:              "/",
		MimeType:           driveFolderMimeType,
		LastViewedByMeDate: string(launch),
		ModifiedDate:       string(launch),
		CreatedDate:        string(launch),
	}
	// Inode allocation special-cases the rootId, so we can let the usual
	// code paths do all the work
	_, err := d.UpdateFile(nil, file)
	return err
}

// Delete all the stored metadata from Google Drive, preserving only the
// mappings of fileid to inodes. Cached chunks are left behind, but the
// blocks will be recycled.
func (d *DriveDB) reinit() error {
	d.Lock()
	defer d.Unlock()
	i := d.cpt.LastInode    // preserve the last Inode allocated
	d.cpt = NewCheckpoint() // recreate the checkpoint
	d.cpt.LastInode = i     // restore the last Inode allocated
	s := time.Now()
	err := d.RemoveAllFiles() // blow away all of the metadata from Drive
	debug.Printf("Removing all files took %v seconds.", time.Since(s))
	return err
}

// LevelDB key helpers. Key prefixes are 3 chars and ":".
func internalKey(key string) []byte {
	return []byte("int:" + key)
}

func fileIdToInodeKey(key string) []byte {
	return []byte("f2i:" + key)
}

func inodeToFileIdKey(key uint64) []byte {
	return []byte("i2f:" + fmt.Sprintf("%d", key))
}

func fileKey(key string) []byte {
	return []byte("fid:" + key)
}

func childKey(key string) []byte {
	return []byte("kid:" + key)
}

func cacheMapKeyPrefix(fileId string) []byte {
	return []byte(fmt.Sprintf("cky:%s\\0", fileId))
}

func cacheMapKey(fileId string, chunk int64) []byte {
	return []byte(fmt.Sprintf("cky:%s\\0%d", fileId, chunk))
}

func deKey(key string) string {
	return key[4:]
}

// get retrives a single key from the database.
func (d *DriveDB) get(key []byte, item interface{}) error {
	data, err := d.db.Get(key, nil)
	if err != nil {
		return err
	}
	return decode(data, item)
}

// writeCheckpoint writes the checkpoint to the db, optionally using a batch.
func (d *DriveDB) writeCheckpoint(batch *leveldb.Batch) error {
	d.Lock()
	cpt := d.cpt
	d.Unlock()
	bytes, err := encode(cpt)
	if err != nil {
		log.Printf("error encoding checkpoint: %v", err)
		return err
	}
	if batch != nil {
		batch.Put(internalKey("checkpoint"), bytes)
		return nil
	}
	return d.db.Put(internalKey("checkpoint"), bytes, nil)
}

// lastChangeId() returns the last changeID recorded in the checkpoint.
func (d *DriveDB) lastChangeId() int64 {
	d.Lock()
	defer d.Unlock()
	return d.cpt.LastChangeID
}

// setLastChangeId sets the lastChangeId in the checkpoint.
// It does not commit to leveldb; use writeCheckpoint to do that.
func (d *DriveDB) setLastChangeId(id int64) {
	d.Lock()
	defer d.Unlock()
	d.cpt.LastChangeID = id
}

// nextInode allocates a new inode number and updates the checkpoint, including writing to leveldb.
func (d *DriveDB) nextInode(batch *leveldb.Batch) (uint64, error) {
	var inode uint64
	d.Lock()
	d.cpt.LastInode++
	inode = d.cpt.LastInode
	d.Unlock()
	return inode, d.writeCheckpoint(batch)
}

// nextCacheBlock allocates a new cache block number and updates, including writing to leveldb.
func (d *DriveDB) nextCacheBlock(batch *leveldb.Batch) (int64, error) {
	var block int64
	d.Lock()
	block = d.cpt.CacheBlock
	d.cpt.CacheBlock++
	d.Unlock()
	return block, d.writeCheckpoint(batch)
}

// InodeForFileId returns a File's inode number, given its ID.
// Allocates a new inode number if needed, and commits immediately
// to leveldb.
func (d *DriveDB) InodeForFileId(fileId string) (uint64, error) {
	key := "inf:" + fileId
	v, err := d.sf.Do(key, func() (interface{}, error) {
		return d.inodeForFileIdImpl(fileId)
	})
	return v.(uint64), err
}

func (d *DriveDB) inodeForFileIdImpl(fileId string) (uint64, error) {
	var inode uint64
	batch := new(leveldb.Batch)

	// Check if an inode has been allocated for this fileId
	if fileId == d.rootId {
		inode = 1
	} else {
		err := d.get(fileIdToInodeKey(fileId), &inode)
		if err != nil {
			// if not, allocate an inode number
			inode, err = d.nextInode(batch)
			if err != nil {
				return 0, err
			}
		}
	}

	// Check the opposite mapping is present and correct
	var currentId string
	err := d.get(inodeToFileIdKey(inode), &currentId)
	if err == nil {
		if currentId == fileId {
			return inode, nil
		} else {
			debug.Printf("inodeToFileId mapping wrong for %v, expected %v got %v", inode, fileId, currentId)
		}
	}

	encodedInode, err := encode(inode)
	if err != nil {
		return 0, err
	}

	encodedFileId, err := encode(fileId)
	if err != nil {
		return 0, err
	}

	// Create forward and reverse mappings.
	batch.Put(fileIdToInodeKey(fileId), encodedInode)
	batch.Put(inodeToFileIdKey(inode), encodedFileId)
	err = d.db.Write(batch, nil)
	if err != nil {
		return 0, err
	}
	return inode, nil
}

// AllFileIds returns the IDs of all Google Drive file objects currently stored.
func (d *DriveDB) AllFileIds() ([]string, error) {
	var ids []string
	// We can't Close() until all iterators are released.
	// TODO: this can still be racy with Close(), fix that.
	d.iters.Add(1)
	iter := d.db.NewIterator(util.BytesPrefix(fileKey("")), nil)
	for iter.Next() {
		ids = append(ids, deKey(string(iter.Key())))
	}
	iter.Release()
	d.iters.Done()
	return ids, iter.Error()
}

// ChildFileIds returns the IDs of all Files that have parent refs to the given file.
func (d *DriveDB) ChildFileIds(fileId string) ([]string, error) {
	var ids []string
	d.iters.Add(1)
	batch := new(leveldb.Batch)
	iter := d.db.NewIterator(util.BytesPrefix(childKey(fileId)), nil)
	for iter.Next() {
		pidcid := deKey(string(iter.Key()))
		cid := pidcid[len(fileId)+1:]
		found, err := d.db.Has(fileKey(cid), nil)
		if err == nil && found {
			ids = append(ids, cid)
		} else {
			batch.Delete(iter.Key())
		}
	}
	iter.Release()
	d.iters.Done()
	if batch.Len() > 0 {
		err := d.db.Write(batch, nil)
		if err != nil {
			log.Printf("error writing to db: %v", err)
		}
	}
	return ids, iter.Error()
}

// FileById returns a File, given its ID.
func (d *DriveDB) FileById(fileId string) (*gdrive.File, error) {
	var res gdrive.File
	err := d.get(fileKey(fileId), &res)
	if err != nil {
		return nil, err
	}
	return &res, nil
}

// FileIdForInode returns the FileId associated with a given inode.
func (d *DriveDB) FileIdForInode(inode uint64) (string, error) {
	var fileId string
	err := d.get(inodeToFileIdKey(inode), &fileId)
	if err != nil {
		log.Printf("FileIdForInode: %v: %v", inode, err)
		return "", err
	}
	return fileId, nil
}

// FileByInode returns a *File given an inode number
func (d *DriveDB) FileByInode(inode uint64) (*File, error) {
	f, ok := d.lruCache.Get(inode)
	if ok {
		return f.(*File), nil
	}

	fileId, err := d.FileIdForInode(inode)
	if err != nil {
		return nil, err
	}
	file, err := d.FileByFileId(fileId)
	if err != nil {
		return nil, err
	}
	d.lruCache.Add(inode, file)
	return file, nil
}

// FileByFileId returns a *File given a fileId
func (d *DriveDB) FileByFileId(fileId string) (*File, error) {
	gdriveFile, err := d.FileById(fileId)
	if err != nil {
		return nil, fmt.Errorf("unknown fileId %v: %v", fileId, err)
	}

	file := File{gdriveFile, 0, nil, "", time.Time{}}
	file.Inode, err = d.InodeForFileId(fileId)
	if err != nil {
		return nil, fmt.Errorf("no inode for %v: %v", fileId, err)
	}

	childFileIds, err := d.ChildFileIds(fileId)
	if err != nil {
		return nil, fmt.Errorf("error getting children of fileId %v: %v", fileId, err)
	}
	file.Children = make([]uint64, len(childFileIds))
	for i, fileId := range childFileIds {
		inode, err := d.InodeForFileId(fileId)
		if err != nil {
			return nil, fmt.Errorf("error getting inode of child %v: %v", fileId, err)
		}
		file.Children[i] = inode
	}
	return &file, nil
}

// Refresh the file object of the given fileId
func (d *DriveDB) Refresh(fileId string) (*File, error) {
	f, err := d.service.Files.Get(fileId).Do()
	if err != nil {
		return &File{}, err
	}
	return d.UpdateFile(nil, f)
}

// The DownloadUrl has a finite lifetime, this ensures we have a fresh cached copy
// hint: "403 Forbidden" is returned when it has expired
func (d *DriveDB) FreshDownloadUrl(f *File) string {
	if f.DownloadUrl == "" {
		return ""
	}
	if time.Since(f.cachedDownloadUrlTime) < downloadUrlLifetime {
		return f.cachedDownloadUrl
	}
	log.Printf("Refreshing DownloadUrl for %v", f.Title)
	fresh, err := d.service.Files.Get(f.Id).Do()
	if err != nil {
		log.Printf("Failed to refresh DownloadUrl: %v", err)
		return f.DownloadUrl
	}
	f.cachedDownloadUrl = fresh.DownloadUrl
	f.cachedDownloadUrlTime = time.Now()
	log.Printf("Cached DownloadUrl for %v for %v", f.Title, downloadUrlLifetime)
	return fresh.DownloadUrl
}

// RemoveAllFiles removes all file entries and child references from leveldb.
// This also flushes the cache, but preserves the fileid->inode mapping
func (d *DriveDB) RemoveAllFiles() error {
	af, err := d.AllFileIds()
	if err != nil {
		return fmt.Errorf("AllFileIds(): %v", err)
	}
	batch := new(leveldb.Batch)
	for _, id := range af {
		d.RemoveFileById(id, batch)
	}
	err = d.db.Write(batch, nil)
	if err != nil {
		return err
	}
	return nil
}

func (d *DriveDB) RemoveFile(f *gdrive.File) error {
	if f == nil {
		return nil
	}
	return d.RemoveFileById(f.Id, nil)
}

func (d *DriveDB) RemoveFileById(fileId string, batch *leveldb.Batch) error {
	if batch == nil {
		batch = new(leveldb.Batch)
	}
	// delete the file itself.
	batch.Delete(fileKey(fileId))
	// and its cached data.
	d.clearDataCache(fileId)
	// clear the inode cache.
	inode, err := d.InodeForFileId(fileId)
	if err == nil {
		// remove from the cache
		d.lruCache.Remove(inode)
	}
	// delete the inode to fileid mapping
	batch.Delete(inodeToFileIdKey(inode))
	// nota bene: fileid to inode mapping is preserved, in case we see this
	// fileid again in the future; preserves mapping during re-init

	// also delete all of its child refs
	d.iters.Add(1)
	iter := d.db.NewIterator(util.BytesPrefix(childKey(fileId)), nil)
	for iter.Next() {
		batch.Delete(iter.Key())
	}
	iter.Release()
	d.iters.Done()
	// and delete any parents' refs to it.
	f, err := d.FileById(fileId)
	if err == nil && f != nil {
		for _, pr := range f.Parents {
			batch.Delete(childKey(pr.Id + ":" + fileId))
		}
	}
	err = d.db.Write(batch, nil)
	if err != nil {
		return err
	}
	return nil
}

// UpdateFile commits a gdrive.File to levelDB, updating all mappings and allocating inodes if needed.
func (d *DriveDB) UpdateFile(batch *leveldb.Batch, f *gdrive.File) (*File, error) {
	if f == nil {
		return &File{}, fmt.Errorf("cannot update nil File")
	}
	fileId := f.Id
	bytes, err := encode(f)
	if err != nil {
		return &File{}, fmt.Errorf("error encoding file %v: %v", fileId, err)
	}

	clearFromCache := []string{f.Id}

	b := batch
	if b == nil {
		b = new(leveldb.Batch)
	}

	// Find its inode, allocate if necessary
	inode, err := d.InodeForFileId(fileId)
	if err != nil {
		return &File{}, fmt.Errorf("error allocating inode for fileid %v: %v", fileId, err)
	}

	// Grab a copy of the file object as it existed previously, if it did
	var oldParents map[string]bool
	of, err := d.FileById(fileId)
	if err == nil {
		oldParents = make(map[string]bool, len(of.Parents))
		for _, pr := range of.Parents {
			oldParents[pr.Id] = true
		}
	}

	// write the file itself.
	b.Put(fileKey(fileId), bytes)

	// Maintain child references
	for _, pr := range f.Parents {
		debug.Printf("Adding parent: %v", pr.Id)
		b.Put(childKey(pr.Id+":"+fileId), nil) // we care only about the key
		clearFromCache = append(clearFromCache, pr.Id)
		delete(oldParents, pr.Id)
	}

	for pId := range oldParents { // these parents were no longer present
		debug.Printf("Removing parent: %v", pId)
		clearFromCache = append(clearFromCache, pId)
		b.Delete(childKey(pId + ":" + fileId))
	}

	// Write now if no batch was supplied.
	if batch == nil {
		err := d.db.Write(b, nil)
		if err != nil {
			return &File{}, err
		}
	}

	// Wipe the lru cache. We'll re-read elsewhere if needed.
	for _, fileId := range clearFromCache {
		inode, err := d.InodeForFileId(fileId)
		if err != nil {
			debug.Printf("error flushing fileId %v from cache: %v", fileId, err)
		}
		d.lruCache.Remove(inode)
	}

	file := File{f, inode, nil, "", time.Time{}}
	d.clearDataCache(fileId)
	return &file, nil
}

func (d *DriveDB) FlushCachedInode(inode uint64) {
	d.lruCache.Remove(inode)
}

// pollForChanges is a background goroutine to poll Drive for changes.
func (d *DriveDB) pollForChanges() {
	poll := make(chan struct{})
	pollTime := time.NewTicker(d.pollInterval).C
	http.HandleFunc("/refresh", func(w http.ResponseWriter, r *http.Request) {
		poll <- struct{}{}
		fmt.Fprintf(w, "Refresh request accepted.")
	})
	// TODO: Allow full requery via http handler, invoke on leveldb corruption
	// track lastChangeId outside of readChanges, just pass in 0 to rebuild

	d.readChanges()
	for {
		select {
		case <-pollTime:
			d.readChanges()
		case <-poll:
			d.readChanges()
		}
	}
}

// readChanges is called by pollForChanges to grab all new metadata changes from Drive.
func (d *DriveDB) readChanges() {
	l := d.service.Changes.List().IncludeDeleted(true).IncludeSubscribed(true).MaxResults(1000)
	lastChangeId := d.lastChangeId()

	if lastChangeId > 0 {
		l.StartChangeId(lastChangeId + 1)
	}

	debug.Printf("Querying Google Drive for changes since %d.", lastChangeId)
	var filenum int
	for {
		filenum++
		c, err := l.Do()
		if err != nil {
			log.Printf("sync error: %v", err)
			return
		}
		debug.Printf("Response from Drive contains %d changes of %d", len(c.Items), c.LargestChangeId)
		if *logChanges {
			filename := fmt.Sprintf("%s/changes.out.%d", d.dbpath, filenum)
			data, _ := encode(c)
			ioutil.WriteFile(filename, data, 0700)
		}

		// Process the changelist.
		d.changes <- c

		if len(c.Items) == 0 {
			return
		}

		// Go to the next page, or next syncid.
		if c.NextPageToken != "" {
			l.PageToken(c.NextPageToken)
		} else {
			return
		}
	}
}

// processChange applies a ChangeList to the database.
func (d *DriveDB) processChange(c *gdrive.ChangeList) error {
	if c == nil {
		return nil
	}

	// If we read zero items, there's no work to do, and we're probably synced.
	if len(c.Items) == 0 {
		if d.lastChangeId() >= c.LargestChangeId {
			d.synced.Broadcast()
		}
		return nil
	}

	log.Printf("processing %v/%v, %v changes", d.lastChangeId(), c.LargestChangeId, len(c.Items))

	batch := new(leveldb.Batch)
	for _, i := range c.Items {
		batch.Reset()
		// Wipe the lru cache for this file. We'll re-read elsewhere if needed.
		inode, err := d.InodeForFileId(i.FileId)
		if err != nil && inode > 0 {
			d.lruCache.Remove(inode)
		}
		// Update leveldb.
		// TODO: don't delete trashed/hidden files? ".trash" folder?
		if i.Deleted || i.File.Labels.Trashed || i.File.Labels.Hidden {
			d.RemoveFileById(i.FileId, batch)
		} else {
			d.UpdateFile(batch, i.File)
		}
		// Update the checkpoint, which now encompasses one additional change.
		d.setLastChangeId(i.Id)
		err = d.writeCheckpoint(batch)
		if err != nil {
			return err
		}
		// Commit
		err = d.db.Write(batch, nil)
		if err != nil {
			return err
		}
	}
	// Signal we're synced, if we are.
	if d.lastChangeId() >= c.LargestChangeId {
		d.synced.Broadcast()
	}
	return nil
}

// sync is a background goroutine to sync drive data.
func (d *DriveDB) sync() {
	var c *gdrive.ChangeList
	for {
		c = <-d.changes
		err := d.processChange(c)
		if err != nil {
			// TODO: trigger reinit(), unless rate > N, then log.Fatal
			log.Printf("error evaluating change from drive: %v", err)
		}
	}
}

// WaitUntilSynced blocks until are are synced with Drive.
func (d *DriveDB) WaitUntilSynced() {
	d.synced.L.Lock()
	d.synced.Wait()
	d.synced.L.Unlock()
}

// Close closes DriveDB, waiting until all iterators are closed.
func (d *DriveDB) Close() {
	d.iters.Wait()
	d.db.Close()
	d.db = nil
}

// Data is read from drive and cached on disk. The Drive read size is intended to be larger
// than the cache size, to account for higher latency to Drive than local disk.
// Chunks of dbDataChunkSize are stored in files next to the leveldb. Records in the leveldb
// point from FileID and chunk number to a cache file on disk. The disk cache works like a ring
// buffer.

// Map an offset and a size to low and high chunk numbers.
func (d *DriveDB) chunkNumbers(offset, size int64) (chunk0, chunkN int64) {
	chunk0 = offset / dbDataChunkSize              // lowest chunk number encompassing the offset
	chunkN = (offset + size - 1) / dbDataChunkSize // highest chunk number, encompassing offset+size.
	return
}

func (d *DriveDB) chunkToDriveChunk(chunk int64) int64 {
	return chunk * dbDataChunkSize / d.driveSize
}

func (d *DriveDB) driveChunkToChunk(dchunk int64) int64 {
	return dchunk * d.driveSize / dbDataChunkSize
}

// ReadFiledata reads a chunk of a file, possibly from cache.
func (d *DriveDB) ReadFiledata(fileId string, offset, size, filesize int64) ([]byte, error) {
	var ret []byte
	// Read all the necessary chunks
	chunk0, chunkN := d.chunkNumbers(offset, size)
	for chunk := chunk0; chunk <= chunkN; chunk++ {
		data, err := d.readChunk(fileId, chunk, filesize)
		if err != nil {
			log.Printf(" chunk %v read error: %v", chunk, err)
			continue
		}
		ret = append(ret, data...)
	}

	// We may have too much data here -- before offset and after end. Return an appropriate slice.
	low := offset - chunk0*dbDataChunkSize
	if low < 0 {
		low = 0
	}
	high := low + size
	dsize := int64(len(ret))
	if high > dsize {
		high = dsize
	}
	buf := ret[low:high]
	return buf, nil
}

// clearDataCache removes the leveldb block cache records, but leaves the actual
// blocks on disk. The blocks will be recycled, so this ok.
func (d *DriveDB) clearDataCache(fileId string) {
	var ids []string
	d.iters.Add(1)
	iter := d.db.NewIterator(util.BytesPrefix(cacheMapKeyPrefix(fileId)), nil)
	for iter.Next() {
		ids = append(ids, string(iter.Key()))
	}
	iter.Release()
	d.iters.Done()
	batch := new(leveldb.Batch)
	for _, id := range ids {
		batch.Delete([]byte(id))
	}
	d.db.Write(batch, nil)
}

// readChunk singleflights the read of a chunk of data from a drive file.
func (d *DriveDB) readChunk(fileId string, chunk, filesize int64) ([]byte, error) {
	if chunk*dbDataChunkSize > filesize {
		return nil, fmt.Errorf("read past eof")
	}
	key := cacheMapKey(fileId, chunk)
	v, err := d.sf.Do(string(key), func() (interface{}, error) {
		return d.readChunkImpl(fileId, chunk, filesize)
	})
	return v.([]byte), err
}

// blockFilename returns the filename at which a cache block can be found.
// It creates directories as needed.
func (d *DriveDB) blockFilename(block int64) (string, error) {
	keysize := len(fmt.Sprintf("%d", *cacheSize))
	f := fmt.Sprintf("%%0%dd", keysize)
	b := fmt.Sprintf(f, block)
	dir := path.Join(d.data, b[:keysize/2])
	err := os.MkdirAll(dir, 0700)
	if err != nil {
		return "", err
	}
	p := path.Join(dir, b)
	return p, nil
}

func (d *DriveDB) writeCacheBlock(batch *leveldb.Batch, fileId string, chunk int64, data []byte) error {
	// See if there's an existing block.
	cacheKey := cacheMapKey(fileId, chunk)
	var block int64
	err := d.get(cacheKey, &block)
	if err != nil {
		// get a new block number
		block, err = d.nextCacheBlock(batch)
		if err != nil {
			return fmt.Errorf("block allocation failed: %v", err)
		}
		bytes, err := encode(block)
		if err != nil {
			return fmt.Errorf("block encode failed: %v", err)
		}
		batch.Put(cacheKey, bytes)
	}
	name, err := d.blockFilename(block)
	if err != nil {
		return err
	}
	f, err := os.Create(name)
	if err != nil {
		return err
	}
	defer f.Close()
	written, err := f.Write(cacheKey)
	if err != nil || written != len(cacheKey) {
		return fmt.Errorf("failed to write cache chunk %v of %s", chunk, fileId)
	}
	written, err = f.Write(data)
	if err != nil || written != len(data) {
		return fmt.Errorf("failed to write cache chunk %v of %s", chunk, fileId)
	}
	return nil
}

func (d *DriveDB) readCacheBlock(fileId string, chunk int64) ([]byte, error) {
	// Check the db for a cache chunk
	cacheKey := cacheMapKey(fileId, chunk)
	var block int64
	err := d.get(cacheKey, &block)
	if err != nil {
		return nil, err
	}
	// Try to read the file.
	name, err := d.blockFilename(block)
	if err != nil {
		_ = d.db.Delete(cacheKey, nil)
		return nil, err
	}
	data, err := ioutil.ReadFile(name)
	if err != nil {
		_ = d.db.Delete(cacheKey, nil)
		return nil, err
	}
	if len(data) <= len(cacheKey) {
		_ = d.db.Delete(cacheKey, nil)
		return nil, fmt.Errorf("empty block? %s, %v", fileId, chunk)
	}
	// Check for the fileId
	if bytes.Compare(data[:len(cacheKey)], cacheKey) != 0 {
		_ = d.db.Delete(cacheKey, nil)
		return nil, fmt.Errorf("mismatched fileId in cache chunk: %s, %v", fileId, chunk)
	}
	return data[len(cacheKey):], nil
}

func (d *DriveDB) writeChunks(fileId string, drivechunk int64, data []byte) error {
	size := len(data)
	if size == 0 {
		return nil
	}
	// Split up the retrieved chunk into dbDataChunkSize segments and store them in the cache.
	chunks := size / dbDataChunkSize
	batch := new(leveldb.Batch)
	basechunk := (drivechunk * d.driveSize) / dbDataChunkSize
	for c := 0; c <= chunks; c++ {
		// base chunk number plus current block number
		cnum := basechunk + int64(c)
		// data segment
		start := int64(c) * dbDataChunkSize
		end := int64(c+1) * dbDataChunkSize
		if end > int64(size) {
			end = int64(size)
		}
		if start >= end {
			continue
		}
		datasegment := data[start:end]
		err := d.writeCacheBlock(batch, fileId, cnum, datasegment)
		if err != nil {
			return err
		}
	}
	return d.db.Write(batch, nil)
}

// readChunkImpl actually reads the data from either the db or drive.
func (d *DriveDB) readChunkImpl(fileId string, chunk, filesize int64) ([]byte, error) {

	data, err := d.readCacheBlock(fileId, chunk)
	if err == nil {
		d.prefetchDriveChunk(fileId, chunk, filesize)
		return data, nil
	}

	// map to larger drive read size
	dchunk := d.chunkToDriveChunk(chunk)
	data, err = d.getChunkFromDrive(fileId, dchunk, filesize)
	if err != nil {
		log.Printf("error reading from drive: %v", err)
		return nil, err
	}

	size := int64(len(data))
	// map back to cache chunk size and extract the requested segment
	start := chunk*dbDataChunkSize - dchunk*d.driveSize
	end := start + int64(dbDataChunkSize)
	if end > size {
		end = size
	}
	buf := data[start:end]
	d.prefetchDriveChunk(fileId, chunk, filesize)
	return buf, d.writeChunks(fileId, dchunk, data)
}

func (d *DriveDB) prefetcher() {
	for {
		select {
		case s := <-d.pfetchq:
			// See if the next chunk is already cached.
			newchunk := d.chunkToDriveChunk(s.chunk) + 1
			c := d.driveChunkToChunk(newchunk)
			_, err := d.readCacheBlock(s.fileId, c)
			if err == nil {
				continue
			}
			// if it isn't, get it.
			data, err := d.getChunkFromDrive(s.fileId, newchunk, s.filesize)
			if err != nil {
				log.Printf("prefetch error: %v", err)
				continue
			}
			d.writeChunks(s.fileId, newchunk, data)
		}
	}
}

// readahead the next drive chunk if needed, async
func (d *DriveDB) prefetchDriveChunk(fileId string, chunk, filesize int64) {
	// See if the next chunk is already cached.
	newchunk := d.chunkToDriveChunk(chunk) + 1
	if newchunk*d.driveSize > filesize {
		return
	}
	c := d.driveChunkToChunk(newchunk)
	_, err := d.readCacheBlock(fileId, c)
	if err == nil {
		return
	}

	// if not, queue it.
	d.pfetchq <- DownloadSpec{
		fileId:   fileId,
		chunk:    chunk,
		filesize: filesize,
	}
}

// singleflight drive fetches.
func (d *DriveDB) getChunkFromDrive(fileId string, chunk, filesize int64) ([]byte, error) {
	v, err := d.sf.Do(fmt.Sprintf("%s/%v", fileId, chunk), func() (interface{}, error) {
		return d.getChunkFromDriveImpl(fileId, chunk, filesize)
	})
	return v.([]byte), err
}

// getChunkFromDriveImpl gets a drive-chunk (larger than cache-chunk) from Drive.
func (d *DriveDB) getChunkFromDriveImpl(fileId string, chunk, filesize int64) ([]byte, error) {
	f, err := d.FileByFileId(fileId)
	if err != nil {
		return nil, err
	}
	url := d.FreshDownloadUrl(f)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	// See http://tools.ietf.org/html/rfc2616#section-14.35  (.1 and .2)
	// https://developers.google.com/drive/web/manage-downloads#partial_download
	start := chunk * d.driveSize
	if start > filesize {
		return nil, fmt.Errorf("chunk %v: requested %v, after EOF %v", chunk, start, filesize)
	}
	end := (chunk+1)*d.driveSize - 1
	if end > filesize {
		end = filesize
	}
	spec := fmt.Sprintf("bytes=%d-%d", start, end)
	req.Header.Add("Range", spec)

	resp, err := d.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("client.Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 206 && resp.StatusCode != 200 {
		err := fmt.Errorf("for %s got HTTP status %v, want 206 or 200: %v", spec, resp.StatusCode, resp.Status)
		return nil, err
	}
	chunkBytes, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("ioutil.ReadAll: %v", err)
	}
	return chunkBytes, nil
}
