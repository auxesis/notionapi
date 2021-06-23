package notionapi

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kjk/siser"
)

const (
	recCacheName = "noahttpcache"
)

// RequestCacheEntry has info about request (method/url/body) and response
type RequestCacheEntry struct {
	// request info
	Method string
	URL    string
	Body   []byte
	// response
	Response []byte
}

// EventDidDownload is for logging. Emitted when page
// or file is downloaded
type EventDidDownload struct {
	// if page, PageID is set
	PageID string
	// if file, URL is set
	FileURL string
	// how long did it take to download
	Duration time.Duration
}

// EventError is for logging. Emitted when there's error to log
type EventError struct {
	Error string
}

// EventDidReadFromCache is for logging. Emitted when page
// or file is read from cache.
type EventDidReadFromCache struct {
	// if page, PageID is set
	PageID string
	// if file, URL is set
	FileURL string
	// how long did it take to download
	Duration time.Duration
}

// EventGotVersions is for logging. Emitted
type EventGotVersions struct {
	Count    int
	Duration time.Duration
}

// CachingClient implements optimized (cached) downloading of pages.
// Cache of pages is stored in CacheDir. We return pages from cache.
// If RedownloadNewerVersions is true, we'll re-download latest version
// of the page (as opposed to returning possibly outdated version
// from cache). We do it more efficiently than just blindly re-downloading.
type CachingClient struct {
	CacheDir string
	client   *Client
	// NoReadCache disables reading from cache i.e. downloaded pages
	// will be written to cache but not read from it
	NoReadCache bool

	// if true, will not make network requests
	NoNetwork bool

	pageIDToEntries map[string][]*RequestCacheEntry
	// we cache requests on a per-page basis
	currPageID *NotionID

	currPageRequests []*RequestCacheEntry

	// stores pages deserialized just from cache
	idToPageFromCache map[string]*Page

	// if true, we'll re-download a page if a newer version is
	// on the server
	RedownloadNewerVersions bool

	// maps id of the page (in the no-dash format) to latest version
	// of the page available on the server.
	// if doesn't exist, we haven't yet queried the server for the
	// version
	idToPageLatestVersion map[string]int64

	didCheckVersionsOfCachedPages bool

	RequestsFromCache      int
	RequestsNotFromCache   int
	RequestsWrittenToCache int

	EventObserver func(interface{})
}

func recGetKey(r *siser.Record, key string, pErr *error) string {
	if *pErr != nil {
		return ""
	}
	v, ok := r.Get(key)
	if !ok {
		*pErr = fmt.Errorf("didn't find key '%s'", key)
	}
	return v
}

func recGetKeyBytes(r *siser.Record, key string, pErr *error) []byte {
	return []byte(recGetKey(r, key, pErr))
}

func serializeCacheEntry(rr *RequestCacheEntry) ([]byte, error) {
	buf := bytes.NewBuffer(nil)
	w := siser.NewWriter(buf)
	w.NoTimestamp = true
	var r siser.Record
	r.Reset()
	r.Write("Method", rr.Method)
	r.Write("URL", rr.URL)
	r.Write("Body", string(rr.Body))
	response := PrettyPrintJS(rr.Response)
	r.Write("Response", string(response))
	r.Name = recCacheName
	_, err := w.WriteRecord(&r)
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func deserializeCacheEntry(d []byte) ([]*RequestCacheEntry, error) {
	br := bufio.NewReader(bytes.NewBuffer(d))
	r := siser.NewReader(br)
	r.NoTimestamp = true
	var err error
	var res []*RequestCacheEntry
	for r.ReadNextRecord() {
		if r.Name != recCacheName {
			return nil, fmt.Errorf("unexpected record type '%s', wanted '%s'", r.Name, recCacheName)
		}
		rr := &RequestCacheEntry{}
		rr.Method = recGetKey(r.Record, "Method", &err)
		rr.URL = recGetKey(r.Record, "URL", &err)
		rr.Body = recGetKeyBytes(r.Record, "Body", &err)
		rr.Response = recGetKeyBytes(r.Record, "Response", &err)
		res = append(res, rr)
	}
	if err != nil {
		return nil, err
	}
	return res, nil
}

/*
func (c *CachingClient) logf(format string, args ...interface{}) {
	c.client.logf(format, args...)
}
*/

func (c *CachingClient) emitEvent(ev interface{}) {
	if c.EventObserver != nil {
		c.EventObserver(ev)
	}
}

func (c *CachingClient) emitError(format string, args ...interface{}) {
	s := format
	if len(args) > 0 {
		s = fmt.Sprintf(format, args...)
	}
	ev := &EventError{
		Error: s,
	}
	c.emitEvent(ev)
}

func (c *CachingClient) vlogf(format string, args ...interface{}) {
	c.client.vlogf(format, args...)
}

func (c *CachingClient) readRequestsCacheFile(dir string) error {
	timeStart := time.Now()
	c.pageIDToEntries = map[string][]*RequestCacheEntry{}
	err := os.MkdirAll(dir, 0755)
	if err != nil {
		return err
	}
	entries, err := ioutil.ReadDir(dir)
	if err != nil {
		return err
	}
	nFiles := 0

	for _, fi := range entries {
		if !fi.Mode().IsRegular() {
			continue
		}
		name := fi.Name()
		if !strings.HasSuffix(name, ".txt") {
			continue
		}
		maybeID := strings.Replace(name, ".txt", "", -1)
		nid := NewNotionID(maybeID)
		if nid == nil {
			continue
		}
		nFiles++
		path := filepath.Join(dir, name)
		d, err := ioutil.ReadFile(path)
		if err != nil {
			return err
		}
		entries, err := deserializeCacheEntry(d)
		if err != nil {
			return err
		}
		c.pageIDToEntries[nid.NoDashID] = entries
	}
	c.vlogf("readRequestsCache() loaded %d files in %s\n", nFiles, time.Since(timeStart))
	return nil
}

func NewCachingClient(cacheDir string, client *Client) (*CachingClient, error) {
	if cacheDir == "" {
		return nil, errors.New("must provide cacheDir")
	}
	if client == nil {
		return nil, errors.New("must provide client")
	}
	res := &CachingClient{
		CacheDir: cacheDir,
		client:   client,
	}
	// TODO: ignore error?
	err := res.readRequestsCacheFile(cacheDir)
	if err != nil {
		return nil, err
	}
	return res, nil
}

func (c *CachingClient) tryReadFromCache(method string, uri string, body []byte) ([]byte, bool) {
	if c.NoReadCache {
		return nil, false
	}
	pageID := c.currPageID.NoDashID
	pageRequests := c.pageIDToEntries[pageID]
	for _, r := range pageRequests {
		if r.Method != method {
			continue
		}
		if r.URL != uri {
			continue
		}
		if !bytes.Equal(r.Body, body) {
			continue
		}
		return r.Response, true
	}
	return nil, false
}

func (c *CachingClient) writeCacheForCurrPage() error {
	var buf []byte

	if len(c.currPageRequests) == 0 {
		return nil
	}
	for _, rr := range c.currPageRequests {
		d, err := serializeCacheEntry(rr)
		if err != nil {
			return err
		}
		buf = append(buf, d...)
	}

	// append to a file for this page
	fileName := c.currPageID.NoDashID + ".txt"
	path := filepath.Join(c.CacheDir, fileName)
	err := ioutil.WriteFile(path, buf, 0644)
	if err != nil {
		// judgement call: delete file if failed to append
		// as it might be corrupted
		// could instead try appendAtomically()
		c.emitError("CachingClient.writeCacheForCurrPage(): ioutil.WriteFile(%s) failed with '%s'\n", fileName, err)
		os.Remove(path)
		return err
	}
	c.RequestsWrittenToCache += len(c.currPageRequests)
	c.vlogf("CachingClient.writeCacheForCurrPage(): wrote %d cached requests to '%s'\n", len(c.currPageRequests), fileName)
	c.currPageRequests = nil
	return nil
}

func (c *CachingClient) doPostMaybeCached(uri string, body []byte) ([]byte, error) {
	d, ok := c.tryReadFromCache("POST", uri, body)
	if ok {
		c.RequestsFromCache++
		return d, nil
	}
	if c.NoNetwork {
		return nil, fmt.Errorf("'%s' failed because network calls disabled", uri)
	}
	d, err := c.client.doPostInternal(uri, body)
	if err != nil {
		return nil, err
	}
	c.RequestsNotFromCache++

	if c.currPageID != nil {
		rr := &RequestCacheEntry{
			Method:   "POST",
			URL:      uri,
			Body:     body,
			Response: d,
		}
		c.currPageRequests = append(c.currPageRequests, rr)
	}

	return d, nil
}

func (c *CachingClient) getClientCopy() *Client {
	var clientCopy = *c.client
	return &clientCopy
}

func (c *CachingClient) getVersionsForPages(ids []string) ([]int64, error) {
	timeStart := time.Now()
	normalizeIDS(ids)
	// using new client to ensure no caching of http requests
	client := c.getClientCopy()
	client.httpPostOverride = nil
	recVals, err := client.GetBlockRecords(ids)
	if err != nil {
		return nil, err
	}
	results := recVals.Results
	if len(results) != len(ids) {
		return nil, fmt.Errorf("getVersionsForPages(): got %d results, expected %d", len(results), len(ids))
	}
	var versions []int64
	for i, rec := range results {
		// res.Value might be nil when a page is not publicly visible or was deleted
		b := rec.Block
		if b == nil {
			versions = append(versions, 0)
			continue
		}
		id := b.ID
		if !isIDEqual(ids[i], id) {
			panic(fmt.Sprintf("got result in the wrong order, ids[i]: %s, id: %s", ids[0], id))
		}
		versions = append(versions, b.Version)
	}
	c.vlogf("CachingClient.getVersionsForPages(): got info about %d pages in %s\n", len(ids), time.Since(timeStart))
	return versions, nil
}

func (c *CachingClient) updateVersionsForPages(ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	sort.Strings(ids)
	timeStart := time.Now()
	versions, err := c.getVersionsForPages(ids)
	if err != nil {
		return fmt.Errorf("d.updateVersionsForPages() for %d pages failed with '%s'", len(ids), err)
	}
	if len(ids) != len(versions) {
		return fmt.Errorf("d.updateVersionsForPages() asked for %d pages but got %d results", len(ids), len(versions))
	}

	ev := &EventGotVersions{
		Count:    len(ids),
		Duration: time.Since(timeStart),
	}
	c.emitEvent(ev)

	for i := 0; i < len(ids); i++ {
		id := ids[i]
		ver := versions[i]
		id = ToNoDashID(id)
		c.idToPageLatestVersion[id] = ver
	}
	return nil
}

// optimization for RedownloadNewerVersions case: check latest
// versions of all cached pages
func (c *CachingClient) checkVersionsOfCachedPages() error {
	if !c.RedownloadNewerVersions {
		return nil
	}
	if c.didCheckVersionsOfCachedPages {
		return nil
	}
	ids := c.GetPageIDs()
	err := c.updateVersionsForPages(ids)
	if err != nil {
		return err
	}
	c.didCheckVersionsOfCachedPages = true
	return nil
}

// see if this page from in-mmemory cache could be a result based on
// RedownloadNewerVersions
func (c *CachingClient) canReturnCachedPage(p *Page) bool {
	if p == nil {
		return false
	}
	if !c.RedownloadNewerVersions {
		return true
	}
	pageID := ToNoDashID(p.ID)
	if _, ok := c.idToPageLatestVersion[pageID]; !ok {
		// we don't know what the latest version is, so download it
		err := c.updateVersionsForPages([]string{pageID})
		if err != nil {
			return false
		}
	}
	newestVer := c.idToPageLatestVersion[pageID]
	pageVer := p.Root().Version
	return pageVer >= newestVer
}

func (c *CachingClient) ReadPageFromCache(pageID string) (*Page, error) {
	// we can ensure we'll read only from cache by disabling network
	prevNoNetwork := c.NoNetwork
	defer func() {
		c.NoNetwork = prevNoNetwork
	}()
	c.NoNetwork = true
	return c.client.DownloadPage(pageID)
}

func (c *CachingClient) getPageFromCache(pageID string) *Page {
	if c.NoReadCache {
		return nil
	}
	c.checkVersionsOfCachedPages()
	p := c.idToPageFromCache[pageID]
	if c.canReturnCachedPage(p) {
		return p
	}
	p, err := c.ReadPageFromCache(pageID)
	if err != nil {
		return nil
	}
	if c.canReturnCachedPage(p) {
		return p
	}
	return nil
}

func (c *CachingClient) DownloadPage(pageID string) (*Page, error) {
	c.currPageRequests = nil
	c.currPageID = NewNotionID(pageID)
	if c.currPageID == nil {
		return nil, fmt.Errorf("'%s' is not a valid notion id", pageID)
	}

	c.checkVersionsOfCachedPages()

	// over-write httpPost only for the duration of client.DownloadPage()
	// that way we don't permanently change the client
	prevOverride := c.client.httpPostOverride
	defer func() {
		// write out cached requests
		// TODO: what happens if only part of requests were from the cache?
		c.writeCacheForCurrPage()
		c.client.httpPostOverride = prevOverride
		c.currPageID = nil
	}()
	c.client.httpPostOverride = c.doPostMaybeCached
	page := c.getPageFromCache(pageID)
	if page != nil {
		return page, nil
	}

	return c.client.DownloadPage(pageID)
}

func (c *CachingClient) DownloadPagesRecursively(startPageID string, afterDownload func(*Page) error) ([]*Page, error) {
	toVisit := []string{startPageID}
	downloaded := map[string]*Page{}
	for len(toVisit) > 0 {
		pageID := ToNoDashID(toVisit[0])
		toVisit = toVisit[1:]
		if downloaded[pageID] != nil {
			continue
		}

		page, err := c.DownloadPage(pageID)
		if err != nil {
			return nil, err
		}
		downloaded[pageID] = page
		if afterDownload != nil {
			err = afterDownload(page)
			if err != nil {
				return nil, err
			}
		}

		subPages := page.GetSubPages()
		toVisit = append(toVisit, subPages...)
	}
	n := len(downloaded)
	if n == 0 {
		return nil, nil
	}
	var ids []string
	for id := range downloaded {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	pages := make([]*Page, n)
	for i, id := range ids {
		pages[i] = downloaded[id]
	}
	return pages, nil
}

// GetPageIDs returns ids of pages in the cache
func (c *CachingClient) GetPageIDs() []string {
	var res []string
	for id := range c.pageIDToEntries {
		res = append(res, id)
	}
	return res
}
