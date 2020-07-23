package cmd

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang/protobuf/proto"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	log "github.com/sirupsen/logrus"
	"github.com/ulranh/sapnwrfc_exporter/internal"
)

type collector struct {
	// possible metric descriptions.
	Desc *prometheus.Desc

	// a parameterized function used to gather metrics.
	stats func() []metricData
}

type metricData struct {
	name       string
	help       string
	metricType string
	stats      []metricRecord
}

type metricRecord struct {
	value       float64
	labels      []string
	labelValues []string
}

// Describe implements prometheus.Collector.
func (c *collector) Describe(ch chan<- *prometheus.Desc) {
	prometheus.DescribeByCollect(c, ch)
}

// Collect implements prometheus.Collector.
func (c *collector) Collect(ch chan<- prometheus.Metric) {
	// Take a stats snapshot.  Must be concurrency safe.
	stats := c.stats()

	var valueType = map[string]prometheus.ValueType{
		"gauge":   prometheus.GaugeValue,
		"counter": prometheus.CounterValue,
	}
	for _, mi := range stats {
		for _, v := range mi.stats {
			m := prometheus.MustNewConstMetric(
				prometheus.NewDesc(low(mi.name), mi.help, v.labels, nil),
				valueType[low(mi.metricType)],
				v.value,
				v.labelValues...,
			)
			ch <- m
		}
	}
}

func newCollector(stats func() []metricData) *collector {
	return &collector{
		stats: stats,
	}
}

// start collector and web server
func (config *Config) web(flags map[string]*string) error {

	var err error
	config.timeout, err = strconv.ParseUint(*flags["timeout"], 10, 0)
	if err != nil {
		exit(fmt.Sprint(" timeout flag has wrong type", err))
	}
	// add missing system data
	config.Systems, err = config.addPasswordData()
	if err != nil {
		log.WithFields(log.Fields{
			"error": err,
		}).Error("Can't add missing system data.")
		return err
	}
	config.timeout, err = strconv.ParseUint(*flags["timeout"], 10, 0)
	if err != nil {
		exit(fmt.Sprint(" timeout flag has wrong type", err))
	}

	stats := func() []metricData {
		data := config.collectMetrics()
		return data
	}

	c := newCollector(stats)
	prometheus.MustRegister(c)

	// start http server
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	server := &http.Server{
		Addr:         ":" + *flags["port"],
		Handler:      mux,
		WriteTimeout: 10 * time.Second,
		ReadTimeout:  10 * time.Second,
	}
	err = server.ListenAndServe()
	if err != nil {
		return errors.Wrap(err, " web - ListenAndServe")
	}

	return nil
}

// start collecting all metrics and fetch the results
func (config *Config) collectMetrics() []metricData {

	mDataC := make(chan metricData, len(config.metrics))
	var wg sync.WaitGroup

	for mPos := range config.metrics {

		wg.Add(1)
		go func(mPos int) {
			defer wg.Done()
			mDataC <- metricData{
				name:       low(config.metrics[mPos].Name),
				help:       config.metrics[mPos].Help,
				metricType: low(config.metrics[mPos].MetricType),
				stats:      config.collectSystemsMetric(mPos),
			}
		}(mPos)
	}

	go func() {
		wg.Wait()
		close(mDataC)
	}()

	var mData []metricData
	for metric := range mDataC {
		mData = append(mData, metric)
	}

	return mData
}

// start collecting metric information for all tenants
func (config *Config) collectSystemsMetric(mPos int) []metricRecord {
	mRecordsC := make(chan []metricRecord, len(config.Systems))
	var wg sync.WaitGroup

	for sPos := range config.Systems {
		wg.Add(1)
		go func(sPos int) {
			defer wg.Done()
			// all values of Metrics.TagFilter must be in Tenants.Tags, otherwise the
			// metric is not relevant for the tenant
			// !!!!!!!!!!!!!!!! kann eventuell hier
			if subSliceInSlice(config.metrics[mPos].TagFilter, config.Systems[sPos].Tags) {
				mRecordsC <- config.collectServersMetric(mPos, sPos, config.getSrvInfo(mPos, sPos))
			} else {
				mRecordsC <- nil
			}
		}(sPos)
	}

	go func() {
		wg.Wait()
		close(mRecordsC)
	}()

	var sData []metricRecord
	for mRecords := range mRecordsC {
		sData = append(sData, mRecords...)
	}
	return sData
}

// get metric data for the system application servers
func (config *Config) collectServersMetric(mPos, sPos int, servers []serverInfo) []metricRecord {

	// !!!!!!!!!!!!!!!!!!!!!
	// servers := config.Systems[sPos].servers
	// if !config.metrics[mPos].AllServers {
	// 	// only one server is needed with option AllServers=false
	// 	servers = config.Systems[sPos].servers[:1]
	// }

	srvCnt := len(servers)
	mRecordsC := make(chan []metricRecord, srvCnt)

	for _, srv := range servers {

		go func(srv serverInfo) {
			mRecordsC <- config.getRfcData(mPos, sPos, srv)
		}(srv)
	}

	i := 0
	var srvData []metricRecord
	timeAfter := time.After(time.Duration(config.timeout) * time.Second)

stopReading:
	for {
		select {
		case mc := <-mRecordsC:
			if mc != nil {
				srvData = append(srvData, mc...)
			}
			i += 1
			if srvCnt == i {
				break stopReading
			}
		case <-timeAfter:
			break stopReading
		}
	}
	return srvData
}

// get data from sap system
func (config *Config) getRfcData(mPos, sPos int, srv serverInfo) []metricRecord {

	// connect to system/server
	// c, err := connect(config.Systems[sPos], config.Systems[sPos].servers[srvPos])
	// if err != nil {
	// 	return nil
	// }

	// all values of Metrics.TagFilter must be in Tenants.Tags, otherwise the
	// metric is not relevant for the tenant
	// !!!!!!!!!!!!!!!! kann eventuell früher behandelt werden
	// if !subSliceInSlice(config.metrics[mPos].TagFilter, config.Systems[sPos].Tags) {
	// 	return nil
	// }

	// check if all configfile param keys are uppercase otherwise the function call returns an error
	for k, v := range config.metrics[mPos].Params {
		upKey := up(k)
		if !(upKey == k) {
			config.metrics[mPos].Params[upKey] = v
			delete(config.metrics[mPos].Params, k)
		}
	}
	// call function module
	rawData, err := srv.conn.Call(up(config.metrics[mPos].FunctionModule), config.metrics[mPos].Params)
	if err != nil {
		log.WithFields(log.Fields{
			"system": config.Systems[sPos].Name,
			"server": srv.name,
			"error":  err,
		}).Error("Can't call function module")
		return nil
	}

	// close connection
	srv.conn.Close()

	// return table- or field metric data
	return config.metrics[mPos].metricData(rawData, config.Systems[sPos], srv.name)
}

// retrieve table data
func (tMetric tableInfo) metricData(rawData map[string]interface{}, system systemInfo, srvName string) []metricRecord {
	var md []metricRecord
	count := make(map[string]float64)

	for _, res := range rawData[up(tMetric.Table)].([]interface{}) {
		line := res.(map[string]interface{})

		if len(tMetric.RowFilter) == 0 || inFilter(line, tMetric.RowFilter) {
			for field, values := range tMetric.RowCount {
				for _, value := range values {
					namePart := low(interface2String(value))
					if "" == namePart {
						log.WithFields(log.Fields{
							"value":  namePart,
							"system": system.Name,
						}).Error("Configfile RowCount: only string and int types are allowed")
						continue
					}

					if strings.HasPrefix(low(interface2String(line[up(field)])), namePart) || "total" == namePart {
						count[low(field)+"_"+namePart]++
					}
				}
			}
		}
	}

	for field, values := range tMetric.RowCount {
		for _, value := range values {
			namePart := low(interface2String(value))

			data := metricRecord{
				labels:      []string{"system", "usage", "server", "count"},
				labelValues: []string{low(system.Name), low(system.Usage), low(srvName), low(field + "_" + namePart)},
				value:       count[low(field)+"_"+namePart],
			}
			md = append(md, data)
		}
	}
	return md
}

// retrieve field data
func (fMetric fieldInfo) metricData(rawData map[string]interface{}, system systemInfo, srvName string) []metricRecord {

	var fieldLabelValues []string
	for _, label := range fMetric.FieldLabels {
		fieldLabelValues = append(fieldLabelValues, low(rawData[up(label)].(string)))
	}

	var md []metricRecord
	labels := append([]string{"system", "usage", "server"}, fMetric.FieldLabels...)
	labelValues := append([]string{low(system.Name), low(system.Usage), low(srvName)}, fieldLabelValues...)

	if len(labels) != len(labelValues) {
		log.WithFields(log.Fields{
			"system": system.Name,
			"server": srvName,
		}).Error("getRfcData: len(labels) != len(labelValues)")
		return nil

	}

	data := metricRecord{
		labels:      labels,
		labelValues: labelValues,
		value:       1,
	}
	md = append(md, data)
	return md
}

// retrieve system servers
func (config *Config) getSrvInfo(mPos, sPos int) []serverInfo {

	c, err := connect(config.Systems[sPos], config.passwords[config.Systems[sPos].Name])
	if err != nil {
		return nil
	}

	// if only one server is needed for metric
	// -> return the standard connection. it will be closed in getRfcData.
	// if !config.metrics[mPos].AllServers {
	// 	return []serverInfo{serverInfo{config.Systems[sPos].Name, c}}
	// }

	params := map[string]interface{}{}
	r, err := c.Call("TH_SERVER_LIST", params)
	if err != nil {
		log.WithFields(log.Fields{
			"system": config.Systems[sPos].Name,
			"error":  err,
		}).Error("Can't call fumo th_server_list")
		return nil
	}

	srvCnt := len(r["LIST"].([]interface{}))

	// if only one server is needed for the metric
	// or if all servers are needed but only one server exists
	// -> return the standard connection. it will be closed in getRfcData.
	if !config.metrics[mPos].AllServers || 1 == srvCnt {
		return []serverInfo{serverInfo{config.Systems[sPos].Name, c}}
	}

	// if more servers exists, they get their own connection below
	// -> the standard connection has to be closed now
	c.Close()

	srvConnC := make(chan serverInfo, srvCnt)
	var wg sync.WaitGroup
	for _, v := range r["LIST"].([]interface{}) {
		wg.Add(1)

		go func(v interface{}) {
			defer wg.Done()

			appl := v.(map[string]interface{})
			info := strings.Split(strings.TrimSpace(appl["NAME"].(string)), "_")

			sys := config.Systems[sPos]
			sys.Server = strings.TrimSpace(info[0])
			sys.Sysnr = strings.TrimSpace(info[2])

			srv, err := connect(sys, config.passwords[config.Systems[sPos].Name])
			if err != nil {
				log.WithFields(log.Fields{
					"server": info[0],
					"error":  err,
				}).Error("error from getServerConnections")
			} else {
				srvConnC <- serverInfo{info[0], srv}
			}
		}(v)
	}

	go func() {
		wg.Wait()
		close(srvConnC)
	}()

	var servers []serverInfo
	for server := range srvConnC {
		servers = append(servers, server)
	}

	// return connections for all servers. they will be closed in getRfcData.
	return servers
}

// add passwords and system servers to config.Systems
func (config *Config) addPasswordData() ([]systemInfo, error) {
	var secret internal.Secret

	if err := proto.Unmarshal(config.Secret, &secret); err != nil {
		log.Fatal("Secret Values don't exist or are corrupted")
		return nil, errors.Wrap(err, " system  - Unmarshal")
	}

	// var passwords []string
	// passwords := make(map[string]string)
	var systemsOk []systemInfo
	for _, system := range config.Systems {

		// decrypt password and add it to system config
		if _, ok := secret.Name[low(system.Name)]; !ok {
			log.WithFields(log.Fields{
				"system": system.Name,
			}).Error("Can't find password for system")
			continue
		}
		systemsOk = append(systemsOk, system)
		pw, err := internal.PwDecrypt(secret.Name[low(system.Name)], secret.Name["secretkey"])
		if err != nil {
			log.WithFields(log.Fields{
				"system": system.Name,
			}).Error("Can't decrypt password for system")
			continue
		}
		// passwords = append(passwords, pw)
		config.passwords[system.Name] = pw

		// retrieve system servers and add them to the system config
		// c, err := connect(system, serverInfo{system.Server, system.Sysnr})
		// if err != nil {
		// 	continue
		// }
		// defer c.Close()

		// params := map[string]interface{}{}
		// r, err := c.Call("TH_SERVER_LIST", params)
		// if err != nil {
		// 	log.WithFields(log.Fields{
		// 		"system": system.Name,
		// 		"error":  err,
		// 	}).Error("Can't call fumo th_server_list")
		// 	continue
		// }

		// for _, v := range r["LIST"].([]interface{}) {
		// 	appl := v.(map[string]interface{})
		// 	info := strings.Split(strings.TrimSpace(appl["NAME"].(string)), "_")
		// 	system.servers = append(system.servers, serverInfo{
		// 		// !!!!! evtl up() nur fuer name -> testen
		// name:  strings.TrimSpace(info[0]),
		// sysnr: strings.TrimSpace(info[2]),
		// })
		// }
	}
	// config.passwords = passwords
	return systemsOk, nil
}
