package main

import (
	"bufio"
	"io"
	"log"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"
)

// Use direct connection after blocked for chouTimeout
const chouTimeout = 2 * time.Minute

type domainSet map[string]bool

// Basically a concurrent map. I don't want to use channels to implement
// concurrent access to this as I'm comfortable to use locks for simple tasks
// like this
type paraDomainSet struct {
	sync.RWMutex
	domainSet
}

func newDomainSet() domainSet {
	return make(map[string]bool)
}

func (ds domainSet) loadDomainList(fpath string) (lst []string, err error) {
	lst, err = loadDomainList(fpath)
	if err != nil {
		return
	}
	// This executes in single goroutine, so no need to use lock
	for _, v := range lst {
		// debug.Println("loaded domain:", v)
		ds[v] = true
	}
	return
}

func (ds domainSet) toSlice() []string {
	l := len(ds)
	lst := make([]string, l, l)

	i := 0
	for k, _ := range ds {
		lst[i] = k
		i++
	}
	return lst
}

func newParaDomainSet() *paraDomainSet {
	return &paraDomainSet{domainSet: newDomainSet()}
}

func (ds *paraDomainSet) add(dm string) {
	ds.Lock()
	ds.domainSet[dm] = true
	ds.Unlock()
}

func (ds *paraDomainSet) has(dm string) bool {
	ds.RLock()
	_, ok := ds.domainSet[dm]
	ds.RUnlock()
	return ok
}

func (ds *paraDomainSet) del(dm string) {
	ds.Lock()
	delete(ds.domainSet, dm)
	ds.Unlock()
}

var blockedDs = newParaDomainSet()
var directDs = newParaDomainSet()

var blockedDomainChanged = false
var directDomainChanged = false

var alwaysBlockedDs = newDomainSet()
var alwaysDirectDs = newDomainSet()
var chouDs = newDomainSet()

// Record when is the domain added to chou domain set
type chouBlockTime struct {
	sync.Mutex
	time map[string]time.Time
}

var chou = chouBlockTime{time: map[string]time.Time{}}

func isHostInAlwaysDs(host string) bool {
	dm := host2Domain(host)
	return alwaysBlockedDs[dm] || alwaysDirectDs[dm]
}

func isHostAlwaysDirect(host string) bool {
	return alwaysDirectDs[host2Domain(host)]
}

func isHostAlwaysBlocked(host string) bool {
	return alwaysBlockedDs[host2Domain(host)]
}

func isHostBlocked(host string) bool {
	dm := host2Domain(host)
	if alwaysDirectDs[dm] {
		return false
	}
	if alwaysBlockedDs[dm] {
		return true
	}
	if chouDs[dm] {
		chou.Lock()
		t, ok := chou.time[dm]
		chou.Unlock()
		if !ok {
			return false
		}
		if time.Now().Sub(t) < chouTimeout {
			return true
		}
		chou.Lock()
		delete(chou.time, dm)
		chou.Unlock()
		debug.Printf("chou domain %s block time unset\n", dm)
		return false
	}
	return blockedDs.has(dm)
}

func isHostDirect(host string) bool {
	dm := host2Domain(host)
	if alwaysDirectDs[dm] {
		return true
	}
	if alwaysBlockedDs[dm] {
		return false
	}
	return directDs.has(dm)
}

func isHostChouFeng(host string) bool {
	return chouDs[host2Domain(host)]
}

// Return true if the host is taken as blocked later
func addBlockedHost(host string) bool {
	dm := host2Domain(host)
	if isHostAlwaysDirect(host) || hostIsIP(host) || dm == "localhost" {
		return false
	}
	if chouDs[dm] {
		// Record blocked time for chou domain, this marks a chou domain as
		// temporarily blocked
		now := time.Now()
		chou.Lock()
		chou.time[dm] = now
		chou.Unlock()
		debug.Printf("chou domain %s blocked at %v\n", dm, now)
	} else if !blockedDs.has(dm) {
		blockedDs.add(dm)
		blockedDomainChanged = true
		debug.Printf("%s added to blocked list\n", dm)
		// Delete this domain from direct domain set
		delDirectDomain(dm)
	}
	return true
}

func delBlockedDomain(dm string) {
	if blockedDs.has(dm) {
		blockedDs.del(dm)
		blockedDomainChanged = true
		debug.Printf("%s deleted from blocked list\n", dm)
	}
}

func addDirectHost(host string) (added bool) {
	dm := host2Domain(host)
	if isHostInAlwaysDs(host) || chouDs[dm] || dm == "localhost" || hostIsIP(host) {
		return
	}
	if !directDs.has(dm) {
		directDs.add(dm)
		directDomainChanged = true
		debug.Printf("%s added to direct list\n", dm)
	}
	// Delete this domain from blocked domain set
	delBlockedDomain(dm)
	return true
}

func delDirectDomain(dm string) {
	if directDs.has(dm) {
		directDs.del(dm)
		directDomainChanged = true
	}
}

func writeBlockedDs() {
	if !config.UpdateBlocked || !blockedDomainChanged {
		return
	}
	writeDomainList(dsFile.blocked, blockedDs.toSlice())
}

func writeDirectDs() {
	if !config.UpdateDirect || !directDomainChanged {
		return
	}
	writeDomainList(dsFile.direct, directDs.toSlice())
}

// filter out domain in blocked and direct domain set.
func filterOutDs(ds domainSet) {
	for k, _ := range ds {
		if blockedDs.domainSet[k] {
			delete(blockedDs.domainSet, k)
			blockedDomainChanged = true
		}
		if directDs.domainSet[k] {
			delete(directDs.domainSet, k)
			directDomainChanged = true
		}
	}
}

// If a domain name appears in both blocked and direct domain set, only keep
// it in the blocked set.
func filterOutBlockedDsInDirectDs() {
	for k, _ := range blockedDs.domainSet {
		if directDs.domainSet[k] {
			delete(directDs.domainSet, k)
			directDomainChanged = true
		}
	}
	for k, _ := range alwaysBlockedDs {
		if alwaysDirectDs[k] {
			errl.Printf("%s in both always blocked and direct domain lists, taken as blocked.\n", k)
			delete(alwaysDirectDs, k)
		}
	}
}

func writeDomainSet() {
	// chou domain set maybe added to blocked site during execution,
	// filter them out before writing back to disk.
	filterOutDs(chouDs)

	writeBlockedDs()
	writeDirectDs()
}

func loadDomainList(fpath string) (lst []string, err error) {
	var exists bool
	if exists, err = isFileExists(fpath); err != nil {
		errl.Printf("Error loading domaint list: %v\n", err)
	}
	if !exists {
		return
	}
	f, err := os.Open(fpath)
	if err != nil {
		errl.Printf("Error opening domain list %s: %v\n", fpath)
		return
	}
	defer f.Close()

	fr := bufio.NewReader(f)
	lst = make([]string, 0)
	var domain string
	for {
		domain, err = ReadLine(fr)
		if err == io.EOF {
			return lst, nil
		} else if err != nil {
			errl.Printf("Error reading domain list %s: %v\n", fpath, err)
			return
		}
		if domain == "" {
			continue
		}
		lst = append(lst, strings.TrimSpace(domain))
	}
	return
}

func mkConfigDir() (err error) {
	if dsFile.dir == "" {
		return
	}
	exists, err := isDirExists(dsFile.dir)
	if err != nil {
		errl.Printf("Error creating config directory: %v\n", err)
		return
	}
	if exists {
		return
	}
	if err = os.Mkdir(dsFile.dir, 0755); err != nil {
		log.Printf("Error create config directory %s: %v\n", dsFile.dir, err)
	}
	return
}

func writeDomainList(fpath string, lst []string) (err error) {
	if err = mkConfigDir(); err != nil {
		return
	}
	tmpPath := path.Join(dsFile.dir, "tmpdomain")
	f, err := os.Create(tmpPath)
	if err != nil {
		errl.Println("Error creating tmp domain list:", err)
		return
	}

	sort.Sort(sort.StringSlice(lst))

	all := strings.Join(lst, newLine)
	f.WriteString(all)
	f.Close()

	if isWindows() {
		// On windows, can't rename to a file which already exists.
		var exists bool
		if exists, err = isFileExists(fpath); err != nil {
			errl.Printf("Error removing domain list: %v\n", err)
			return
		}
		if exists {
			if err = os.Remove(fpath); err != nil {
				errl.Printf("Error removing domain list %s for update: %v\n", fpath, err)
			}
		}
	}
	if err = os.Rename(tmpPath, fpath); err != nil {
		errl.Printf("Error renaming tmp domain list file to %s: %v\n", fpath, err)
	}
	return
}

var topLevelDomain = map[string]bool{
	"ac":  true,
	"co":  true,
	"org": true,
	"com": true,
	"net": true,
	"edu": true,
}

func host2Domain(host string) (domain string) {
	host, _ = splitHostPort(host)
	host = trimLastDot(host)
	lastDot := strings.LastIndex(host, ".")
	if lastDot == -1 {
		return host // simple host name, we should not hanlde this
	}
	// Find the 2nd last dot
	dot2ndLast := strings.LastIndex(host[:lastDot], ".")
	if dot2ndLast == -1 {
		return host
	}

	part := host[dot2ndLast+1 : lastDot]
	// If the 2nd last part of a domain name equals to a top level
	// domain, search for the 3rd part in the host name.
	// So domains like bbc.co.uk will not be recorded as co.uk
	if topLevelDomain[part] {
		dot3rdLast := strings.LastIndex(host[:dot2ndLast], ".")
		if dot3rdLast == -1 {
			return host
		}
		return host[dot3rdLast+1:]
	}
	return host[dot2ndLast+1:]
}

// TODO reload domain list when receives SIGUSR1
// one difficult here is that we may concurrently access maps which is not
// safe.
// Can we create a new domain set first, then change the reference of the original one?
// Domain set reference changing should be atomic.

func loadDomainSet() {
	blockedDs.loadDomainList(dsFile.blocked)
	directDs.loadDomainList(dsFile.direct)
	alwaysBlockedDs.loadDomainList(dsFile.alwaysBlocked)
	alwaysDirectDs.loadDomainList(dsFile.alwaysDirect)
	chouDs.loadDomainList(dsFile.chou)

	filterOutDs(chouDs)
	filterOutDs(alwaysDirectDs)
	filterOutDs(alwaysBlockedDs)
	filterOutBlockedDsInDirectDs()
}
