package backend

import (
	"../util"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"math/rand"
	"net/http"
	"os"
	"stathat.com/c/consistent"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Proxy struct {
	Circles      []*Circle                 `json:"circles"`
	ListenAddr   string                    `json:"listen_addr"`
	DbList       []string                  `json:"db_list"`
	DbMap        map[string]bool           `json:"db_map"`
	DataDir      string                    `json:"data_dir"`
	MlogDir      string                    `json:"mlog_dir"`
	HashKey      string                    `json:"hash_key"`
	VNodeSize    int                       `json:"vnode_size"`
	FlushSize    int                       `json:"flush_size"`
	FlushTime    time.Duration             `json:"flush_time"`
	LogEnabled   bool                      `json:"log_enabled"`
	Username     string                    `json:"username"`
	Password     string                    `json:"password"`
	AuthSecure   bool                      `json:"auth_secure"`
	HTTPSEnabled bool                      `json:"https_enabled"`
	HTTPSCert    string                    `json:"https_cert"`
	HTTPSKey     string                    `json:"https_key"`
	HaAddrs      []string                  `json:"ha_addrs"`
	IsResyncing  bool                      `json:"is_resyncing"`
	MigrateCpus  int                       `json:"migrate_cpus"`
	MigrateStats []map[string]*MigrateInfo `json:"migrate_stats"`
	Lock         *sync.RWMutex             `json:"lock"`
}

type LineData struct {
	Db        string `json:"db"`
	Line      []byte `json:"line"`
	Precision string `json:"precision"`
}

type MigrateInfo struct {
	DatabaseTotal    int32 `json:"database_total"`
	DatabaseDone     int32 `json:"database_done"`
	MeasurementTotal int32 `json:"measurement_total"`
	MeasurementDone  int32 `json:"measurement_done"`
	MigrateCount     int32 `json:"migrate_count"`
	InPlaceCount     int32 `json:"inplace_count"`
}

func NewProxy(file string) (proxy *Proxy, err error) {
	proxy, err = LoadProxyConfig(file)
	if err != nil {
		return
	}
	err = util.MakeDir(proxy.DataDir)
	if err != nil {
		return
	}
	proxy.MigrateStats = make([]map[string]*MigrateInfo, len(proxy.Circles))
	proxy.Lock = &sync.RWMutex{}
	for circleId, circle := range proxy.Circles {
		circle.CircleId = circleId
		proxy.initCircle(circle)
		proxy.initMigrateStats(circle)
	}
	proxy.DbMap = make(map[string]bool)
	for _, db := range proxy.DbList {
		proxy.DbMap[db] = true
	}
	return
}

func LoadProxyConfig(file string) (proxy *Proxy, err error) {
	proxy = &Proxy{}
	f, err := os.Open(file)
	defer f.Close()
	if err != nil {
		return
	}
	dec := json.NewDecoder(f)
	err = dec.Decode(proxy)
	if err != nil {
		return
	}
	if proxy.ListenAddr == "" {
		proxy.ListenAddr = ":7076"
	}
	if proxy.DataDir == "" {
		proxy.DataDir = "data"
	}
	if proxy.MlogDir == "" {
		proxy.MlogDir = "log"
	}
	if proxy.HashKey == "" {
		proxy.HashKey = "idx"
	} else if proxy.HashKey != "idx" && proxy.HashKey != "name" && proxy.HashKey != "url" {
		return nil, errors.New("invalid hash_key, should be idx, name or url")
	}
	if proxy.VNodeSize <= 0 {
		proxy.VNodeSize = 256
	}
	if proxy.FlushSize <= 0 {
		proxy.FlushSize = 10000
	}
	if proxy.FlushTime <= 0 {
		proxy.FlushTime = 1
	}
	if proxy.MigrateCpus <= 0 {
		proxy.MigrateCpus = 1
	}
	err = proxy.CheckBackends()
	if err != nil {
		return
	}
	log.Printf("%d circles loaded from file", len(proxy.Circles))
	for id, circle := range proxy.Circles {
		log.Printf("circle %d: %d backends loaded", id, len(circle.Backends))
	}
	log.Printf("hash key: %s", proxy.HashKey)
	if len(proxy.DbList) > 0 {
		log.Printf("db list: %v", proxy.DbList)
	}
	return
}

func (proxy *Proxy) CheckBackends() error {
	rec := make(map[string]bool)
	for _, circle := range proxy.Circles {
		if len(circle.Backends) == 0 {
			return errors.New("backends cannot be empty")
		}
		for _, backend := range circle.Backends {
			if backend.Name == "" {
				return errors.New("backend name cannot be empty")
			}
			if _, ok := rec[backend.Name]; ok {
				return errors.New("backend name exists: " + backend.Name)
			}
			rec[backend.Name] = true
		}
	}
	return nil
}

func (proxy *Proxy) initCircle(circle *Circle) {
	circle.Router = consistent.New()
	circle.Router.NumberOfReplicas = proxy.VNodeSize
	circle.MapToBackend = make(map[string]*Backend)
	circle.BackendWgMap = make(map[string]*sync.WaitGroup)
	circle.IsMigrating = false
	circle.MigrateWg = &sync.WaitGroup{}
	for idx, backend := range circle.Backends {
		circle.BackendWgMap[backend.Url] = &sync.WaitGroup{}
		proxy.initBackend(circle, backend, idx)
	}
}

func (proxy *Proxy) initBackend(circle *Circle, backend *Backend, idx int) {
	if proxy.HashKey == "name" {
		circle.Router.Add(backend.Name)
		circle.MapToBackend[backend.Name] = backend
	} else if proxy.HashKey == "url" {
		circle.Router.Add(backend.Url)
		circle.MapToBackend[backend.Url] = backend
	} else {
		circle.Router.Add(strconv.Itoa(idx))
		circle.MapToBackend[strconv.Itoa(idx)] = backend
	}

	backend.AuthSecure = proxy.AuthSecure
	backend.BufferMap = make(map[string]*CBuffer)
	backend.Client = NewClient(strings.HasPrefix(backend.Url, "https"))
	backend.Transport = NewTransport(strings.HasPrefix(backend.Url, "https"))
	backend.Active = true
	backend.LockDbMap = make(map[string]*sync.RWMutex)
	backend.LockBuffer = &sync.RWMutex{}
	backend.LockFile = &sync.RWMutex{}
	backend.OpenFile(proxy.DataDir)

	go backend.CheckActiveBackground()
	go backend.FlushBufferBackground(proxy.FlushTime)
	go backend.RewriteBackground()
}

func (proxy *Proxy) initMigrateStats(circle *Circle) {
	circleId := circle.CircleId
	proxy.MigrateStats[circleId] = make(map[string]*MigrateInfo)
	for _, backend := range circle.Backends {
		proxy.MigrateStats[circleId][backend.Url] = &MigrateInfo{}
	}
}

func GetKey(db, meas string) string {
	return fmt.Sprintf("%s,%s", db, meas)
}

func (proxy *Proxy) GetBackends(key string) []*Backend {
	backends := make([]*Backend, 0)
	for _, circle := range proxy.Circles {
		backend := circle.GetBackend(key)
		backends = append(backends, backend)
	}
	return backends
}

func (proxy *Proxy) Query(w http.ResponseWriter, req *http.Request, tokens []string, db string, alterDb bool) (body []byte, err error) {
	if CheckSelectOrShowFromTokens(tokens) {
		var circle *Circle
		badIds := make(map[int]bool)
		for {
			id := rand.Intn(len(proxy.Circles))
			if _, ok := badIds[id]; ok {
				continue
			}
			circle = proxy.Circles[id]
			if circle.IsMigrating {
				badIds[id] = true
				continue
			}
			if circle.CheckStatus() {
				break
			}
			badIds[id] = true
			if len(badIds) == len(proxy.Circles) {
				return nil, errors.New("circles unavailable")
			}
			time.Sleep(time.Microsecond)
		}
		meas, err := GetMeasurementFromTokens(tokens)
		if err == nil {
			// available circle -> key(db,meas) -> backend -> select or show
			key := GetKey(db, meas)
			backend := circle.GetBackend(key)
			proxy.Logf("query circle: %d backend: %s", circle.CircleId, backend.Url)
			return backend.Query(req, w, false)
		} else {
			// available circle -> all backends -> show
			proxy.Logf("query circle: %d", circle.CircleId)
			return circle.Query(w, req, tokens)
		}
	} else if CheckDeleteOrDropMeasurementFromTokens(tokens) {
		// all circles -> key(db,meas) -> backend -> delete or drop
		meas, err := GetMeasurementFromTokens(tokens)
		if err != nil {
			return nil, err
		}
		var reqBodyBytes []byte
		if req.Body != nil {
			reqBodyBytes, _ = ioutil.ReadAll(req.Body)
		}
		key := GetKey(db, meas)
		backends := proxy.GetBackends(key)
		for _, backend := range backends {
			proxy.Logf("query backend: %s", backend.Url)
			req.Body = ioutil.NopCloser(bytes.NewBuffer(reqBodyBytes))
			body, err = backend.Query(req, w, false)
			if err != nil {
				return nil, err
			}
		}
	} else if alterDb {
		// all circles -> all backends -> create or drop database
		for _, circle := range proxy.Circles {
			if !circle.CheckStatus() {
				return nil, errors.New(fmt.Sprintf("circle %d not health", circle.CircleId))
			}
		}
		for _, circle := range proxy.Circles {
			proxy.Logf("query circle: %d", circle.CircleId)
			body, err = circle.Query(w, req, tokens)
			if err != nil {
				return
			}
		}
	}
	return
}

func (proxy *Proxy) WriteData(data *LineData) {
	data.Line = LineToNano(data.Line, data.Precision)
	meas, err := ScanKey(data.Line)
	if err != nil {
		log.Printf("scan key error: %s", err)
		return
	}
	if ScanSpace(data.Line[len(meas):]) != 2 {
		log.Printf("invalid format, drop data: %s %s %s", data.Db, data.Precision, string(data.Line))
		return
	}

	key := GetKey(data.Db, meas)
	backends := proxy.GetBackends(key)
	// fmt.Printf("%s key: %s; backends:", time.Now().Format("2006-01-02 15:04:05"), key)
	// for _, b := range backends {
	//     fmt.Printf(" %s", b.Name)
	// }
	// fmt.Printf("\n")
	if len(backends) == 0 {
		log.Printf("write data: %v, error: get backends length is 0", data)
		return
	}

	if !bytes.HasSuffix(data.Line, []byte("\n")) {
		data.Line = append(data.Line, []byte("\n")...)
	}

	for _, backend := range backends {
		err := backend.WriteBuffer(data, proxy.FlushSize)
		if err != nil {
			log.Printf("write data to buffer error: %s, %s, %s, %s, %s", err, backend.Url, data.Db, data.Precision, string(data.Line))
		}
	}
}

func (proxy *Proxy) Logf(format string, v ...interface{}) {
	if proxy.LogEnabled {
		log.Printf(format, v...)
	}
}

func (proxy *Proxy) GetDatabases() []string {
	for _, circle := range proxy.Circles {
		for _, backend := range circle.Backends {
			if backend.Active {
				return backend.GetDatabases()
			}
		}
	}
	return nil
}

func (proxy *Proxy) GetBackendUrls(backends []*Backend) []string {
	backendUrls := make([]string, len(backends))
	for k, b := range backends {
		backendUrls[k] = b.Url
	}
	return backendUrls
}

func (proxy *Proxy) Migrate(backend *Backend, dstBackends []*Backend, circle *Circle, db, meas string, seconds int) {
	err := circle.Migrate(backend, dstBackends, db, meas, seconds)
	if err != nil {
		util.Mlog.Printf("migrate error:%s src:%s dst:%v circle:%d db:%s measurement:%s seconds:%d", err, backend.Url, proxy.GetBackendUrls(dstBackends), circle.CircleId, db, meas, seconds)
	}
	defer circle.BackendWgMap[backend.Url].Done()
}

func (proxy *Proxy) Rebalance(circleId int, backends []*Backend, databases []string) {
	util.SetMLog(proxy.MlogDir, "rebalance.log")
	util.Mlog.Printf("rebalance start")
	circle := proxy.Circles[circleId]
	proxy.SetMigratingAndBroadcast(circle, true)
	if len(databases) == 0 {
		databases = proxy.GetDatabases()
	}
	proxy.ClearMigrateStats()
	for _, backend := range backends {
		circle.MigrateWg.Add(1)
		go proxy.RebalanceBackend(backend, circle, databases)
	}
	circle.MigrateWg.Wait()
	defer proxy.SetMigratingAndBroadcast(circle, false)
	util.Mlog.Printf("rebalance done")
}

func (proxy *Proxy) RebalanceBackend(backend *Backend, circle *Circle, databases []string) {
	var migrateCount int
	defer circle.MigrateWg.Done()
	circleId := circle.CircleId
	if !backend.Active {
		util.Mlog.Printf("backend not active: %s", backend.Url)
		return
	}

	stats := proxy.MigrateStats[circleId][backend.Url]
	stats.DatabaseTotal = int32(len(databases))
	measuresOfDbs := make([][]string, len(databases))
	for i, db := range databases {
		measuresOfDbs[i] = backend.GetMeasurements(db)
		stats.MeasurementTotal += int32(len(measuresOfDbs[i]))
	}

	for i, db := range databases {
		for j, meas := range measuresOfDbs[i] {
			key := GetKey(db, meas)
			dstBackend := circle.GetBackend(key)
			if dstBackend.Url != backend.Url {
				util.Mlog.Printf("src:%s dst:%s db:%s measurement:%s", backend.Url, dstBackend.Url, db, meas)
				migrateCount++
				circle.BackendWgMap[backend.Url].Add(1)
				go proxy.Migrate(backend, []*Backend{dstBackend}, circle, db, meas, 0)
				if migrateCount%proxy.MigrateCpus == 0 || (i+1 == len(databases) && j+1 == len(measuresOfDbs[i])) {
					circle.BackendWgMap[backend.Url].Wait()
				}
				atomic.AddInt32(&stats.MigrateCount, 1)
			} else {
				atomic.AddInt32(&stats.InPlaceCount, 1)
			}
			atomic.AddInt32(&stats.MeasurementDone, 1)
		}
		atomic.AddInt32(&stats.DatabaseDone, 1)
	}
}

func (proxy *Proxy) Recovery(fromCircleId, toCircleId int, recoveryUrls []string, databases []string) {
	util.SetMLog(proxy.MlogDir, "recovery.log")
	util.Mlog.Printf("recovery start")
	fromCircle := proxy.Circles[fromCircleId]
	toCircle := proxy.Circles[toCircleId]

	proxy.SetMigratingAndBroadcast(toCircle, true)
	if len(databases) == 0 {
		databases = proxy.GetDatabases()
	}
	recoveryUrlMap := make(map[string]bool)
	if len(recoveryUrls) != 0 {
		for _, u := range recoveryUrls {
			recoveryUrlMap[u] = true
		}
	} else {
		for _, b := range toCircle.Backends {
			recoveryUrlMap[b.Url] = true
		}
	}
	proxy.ClearMigrateStats()
	for _, backend := range fromCircle.Backends {
		fromCircle.MigrateWg.Add(1)
		go proxy.RecoveryBackend(backend, fromCircle, toCircle, recoveryUrlMap, databases)
	}
	fromCircle.MigrateWg.Wait()
	defer proxy.SetMigratingAndBroadcast(toCircle, false)
	util.Mlog.Printf("recovery done")
}

func (proxy *Proxy) RecoveryBackend(backend *Backend, fromCircle, toCircle *Circle, recoveryUrlMap map[string]bool, databases []string) {
	var migrateCount int
	defer fromCircle.MigrateWg.Done()
	fromCircleId := fromCircle.CircleId
	if !backend.Active {
		util.Mlog.Printf("backend not active: %s", backend.Url)
		return
	}

	stats := proxy.MigrateStats[fromCircleId][backend.Url]
	stats.DatabaseTotal = int32(len(databases))
	measuresOfDbs := make([][]string, len(databases))
	for i, db := range databases {
		measuresOfDbs[i] = backend.GetMeasurements(db)
		stats.MeasurementTotal += int32(len(measuresOfDbs[i]))
	}

	for i, db := range databases {
		for j, meas := range measuresOfDbs[i] {
			key := GetKey(db, meas)
			dstBackend := toCircle.GetBackend(key)
			if _, ok := recoveryUrlMap[dstBackend.Url]; ok {
				util.Mlog.Printf("src:%s dst:%s db:%s measurement:%s", backend.Url, dstBackend.Url, db, meas)
				migrateCount++
				fromCircle.BackendWgMap[backend.Url].Add(1)
				go proxy.Migrate(backend, []*Backend{dstBackend}, fromCircle, db, meas, 0)
				if migrateCount%proxy.MigrateCpus == 0 || (i+1 == len(databases) && j+1 == len(measuresOfDbs[i])) {
					fromCircle.BackendWgMap[backend.Url].Wait()
				}
				atomic.AddInt32(&stats.MigrateCount, 1)
			} else {
				atomic.AddInt32(&stats.InPlaceCount, 1)
			}
			atomic.AddInt32(&stats.MeasurementDone, 1)
		}
		atomic.AddInt32(&stats.DatabaseDone, 1)
	}
}

func (proxy *Proxy) Resync(databases []string, seconds int) {
	util.SetMLog(proxy.MlogDir, "resync.log")
	util.Mlog.Printf("resync start")
	if len(databases) == 0 {
		databases = proxy.GetDatabases()
	}
	proxy.SetResyncingAndBroadcast(true)
	proxy.ClearMigrateStats()
	for _, circle := range proxy.Circles {
		for _, backend := range circle.Backends {
			circle.MigrateWg.Add(1)
			go proxy.ResyncBackend(backend, circle, databases, seconds)
		}
		circle.MigrateWg.Wait()
		util.Mlog.Printf("circle %d resync done", circle.CircleId)
	}
	defer proxy.SetResyncingAndBroadcast(false)
	util.Mlog.Printf("resync done")
}

func (proxy *Proxy) ResyncBackend(backend *Backend, circle *Circle, databases []string, seconds int) {
	var migrateCount int
	defer circle.MigrateWg.Done()
	circleId := circle.CircleId
	if !backend.Active {
		util.Mlog.Printf("backend not active: %s", backend.Url)
		return
	}

	stats := proxy.MigrateStats[circleId][backend.Url]
	stats.DatabaseTotal = int32(len(databases))
	measuresOfDbs := make([][]string, len(databases))
	for i, db := range databases {
		measuresOfDbs[i] = backend.GetMeasurements(db)
		stats.MeasurementTotal += int32(len(measuresOfDbs[i]))
	}

	for i, db := range databases {
		for j, meas := range measuresOfDbs[i] {
			key := GetKey(db, meas)
			dstBackends := make([]*Backend, 0)
			for _, toCircle := range proxy.Circles {
				if toCircle.CircleId != circleId {
					dstBackend := toCircle.GetBackend(key)
					dstBackends = append(dstBackends, dstBackend)
				}
			}
			if len(dstBackends) > 0 {
				util.Mlog.Printf("src:%s dst:%v db:%s measurement:%s", backend.Url, proxy.GetBackendUrls(dstBackends), db, meas)
				migrateCount++
				circle.BackendWgMap[backend.Url].Add(1)
				go proxy.Migrate(backend, dstBackends, circle, db, meas, seconds)
				if migrateCount%proxy.MigrateCpus == 0 || (i+1 == len(databases) && j+1 == len(measuresOfDbs[i])) {
					circle.BackendWgMap[backend.Url].Wait()
				}
				atomic.AddInt32(&stats.MigrateCount, 1)
			} else {
				atomic.AddInt32(&stats.InPlaceCount, 1)
			}
			atomic.AddInt32(&stats.MeasurementDone, 1)
		}
		atomic.AddInt32(&stats.DatabaseDone, 1)
	}
}

func (proxy *Proxy) Clear(circleId int) {
	util.SetMLog(proxy.MlogDir, "clear.log")
	util.Mlog.Printf("clear start")
	circle := proxy.Circles[circleId]
	proxy.SetMigratingAndBroadcast(circle, true)
	proxy.ClearMigrateStats()
	for _, backend := range circle.Backends {
		circle.MigrateWg.Add(1)
		go proxy.ClearBackend(backend, circle)
	}
	circle.MigrateWg.Wait()
	defer proxy.SetMigratingAndBroadcast(circle, false)
	util.Mlog.Printf("clear done")
}

func (proxy *Proxy) ClearBackend(backend *Backend, circle *Circle) {
	var migrateCount int
	defer circle.MigrateWg.Done()
	circleId := circle.CircleId
	if !backend.Active {
		util.Mlog.Printf("backend not active: %s", backend.Url)
		return
	}

	databases := backend.GetDatabases()
	stats := proxy.MigrateStats[circleId][backend.Url]
	stats.DatabaseTotal = int32(len(databases))
	measuresOfDbs := make([][]string, len(databases))
	for i, db := range databases {
		measuresOfDbs[i] = backend.GetMeasurements(db)
		stats.MeasurementTotal += int32(len(measuresOfDbs[i]))
	}

	for i, db := range databases {
		for j, meas := range measuresOfDbs[i] {
			util.Mlog.Printf("check backend:%s db:%s measurement:%s", backend.Url, db, meas)
			key := GetKey(db, meas)
			dstBackend := circle.GetBackend(key)
			if dstBackend.Url != backend.Url {
				util.Mlog.Printf("backend:%s db:%s measurement:%s should migrate to %s", backend.Url, db, meas, dstBackend.Url)
				migrateCount++
				circle.BackendWgMap[backend.Url].Add(1)
				go func(backend *Backend, circle *Circle, db, meas string) {
					_, err := backend.DropMeasurement(db, meas)
					if err == nil {
						util.Mlog.Printf("clear backend:%s db:%s measurement:%s done", backend.Url, db, meas)
					} else {
						util.Mlog.Printf("clear backend:%s db:%s measurement:%s error: %s", backend.Url, db, meas, err)
					}
					defer circle.BackendWgMap[backend.Url].Done()
				}(backend, circle, db, meas)
				if migrateCount%proxy.MigrateCpus == 0 || (i+1 == len(databases) && j+1 == len(measuresOfDbs[i])) {
					circle.BackendWgMap[backend.Url].Wait()
				}
				atomic.AddInt32(&stats.MigrateCount, 1)
			} else {
				atomic.AddInt32(&stats.InPlaceCount, 1)
			}
			atomic.AddInt32(&stats.MeasurementDone, 1)
		}
		atomic.AddInt32(&stats.DatabaseDone, 1)
	}
}

func (proxy *Proxy) ClearMigrateStats() {
	for _, stats := range proxy.MigrateStats {
		for _, mi := range stats {
			mi.DatabaseTotal = 0
			mi.DatabaseDone = 0
			mi.MeasurementTotal = 0
			mi.MeasurementDone = 0
			mi.MigrateCount = 0
			mi.InPlaceCount = 0
		}
	}
}

func (proxy *Proxy) SetResyncing(resyncing bool) {
	proxy.Lock.Lock()
	defer proxy.Lock.Unlock()
	proxy.IsResyncing = resyncing
}

func (proxy *Proxy) SetMigrating(circle *Circle, migrating bool) {
	proxy.Lock.Lock()
	defer proxy.Lock.Unlock()
	circle.IsMigrating = migrating
}

func (proxy *Proxy) SetResyncingAndBroadcast(resyncing bool) {
	proxy.SetResyncing(resyncing)
	client := NewClient(proxy.HTTPSEnabled)
	for _, addr := range proxy.HaAddrs {
		url := fmt.Sprintf("http://%s/migrate/state?resyncing=%t", addr, resyncing)
		proxy.PostBroadcast(client, url)
	}
}

func (proxy *Proxy) SetMigratingAndBroadcast(circle *Circle, migrating bool) {
	proxy.SetMigrating(circle, migrating)
	client := NewClient(proxy.HTTPSEnabled)
	for _, addr := range proxy.HaAddrs {
		url := fmt.Sprintf("http://%s/migrate/state?circle_id=%d&migrating=%t", addr, circle.CircleId, migrating)
		proxy.PostBroadcast(client, url)
	}
}

func (proxy *Proxy) PostBroadcast(client *http.Client, url string) {
	if proxy.HTTPSEnabled {
		url = strings.Replace(url, "http", "https", 1)
	}
	req, _ := http.NewRequest("POST", url, nil)
	if proxy.Username != "" || proxy.Password != "" {
		SetBasicAuth(req, proxy.Username, proxy.Password, proxy.AuthSecure)
	}
	client.Do(req)
}
