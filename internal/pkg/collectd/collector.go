package collectd

import (
	"context"
	"errors"
	"log"
	"net/http"
	"sync"
	"time"

	"collectd.org/api"
	"collectd.org/network"

	"github.com/fromanirh/virt-collectd-exporter/pkg/nameconv"
	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var UnknownSecurityLevel = errors.New("Unknown security level")

const Name = "virt_collectd_exporter"

type dataCollector interface {
	Configure(Config) error
	Run(context.Context)
	Describe(ch chan<- *prometheus.Desc)
}

type dataSink interface {
	Write(context.Context, *api.ValueList) error
}

type Collector struct {
	ch       chan api.ValueList
	values   map[string]api.ValueList
	rw       *sync.RWMutex
	srcs     []dataCollector
	address  string
	router   *mux.Router
	conv     *nameconv.NameConverter
	debugLog *log.Logger
}

func NewCollector(conf Config) *Collector {
	c := &Collector{
		ch:     make(chan api.ValueList, 0),
		values: make(map[string]api.ValueList),
		rw:     &sync.RWMutex{},
	}
	if conf.CollectdBinaryAddress != "" {
		log.Printf("CollectD binary protocol endpoint: '%s'", conf.CollectdBinaryAddress)
		bin := &binaryProtoCollector{
			address: conf.CollectdBinaryAddress,
			srv: &network.Server{
				Addr:   conf.CollectdBinaryAddress,
				Writer: c,
			},
		}
		c.srcs = append(c.srcs, bin)
	}
	if conf.CollectdJSONAddress != "" {
		log.Printf("CollectD HTTP JSON protocol endpoint: '%s'", conf.CollectdJSONAddress)
		hj := &httpJSONCollector{
			address: conf.CollectdJSONAddress,
			sink:    c,
		}
		c.srcs = append(c.srcs, hj)
	}
	c.conv, _ = nameconv.NewNameConverter(conf.MetricsSource, conf.MetricsPrefix)
	return c
}

func (c *Collector) SetDebugLog(l *log.Logger) *Collector {
	c.debugLog = l
	return c
}

func (c *Collector) Configure(conf Config) error {
	for _, src := range c.srcs {
		if c.debugLog != nil {
			c.debugLog.Printf("Configuring: %#v", src)
		}
		if err := src.Configure(conf); err != nil {
			if c.debugLog != nil {
				c.debugLog.Printf("Configure failed: %s", err)
			}
			return err
		}
	}

	c.address = conf.MetricsAddress
	c.router = mux.NewRouter().StrictSlash(true)
	name := "metrics"
	c.router.
		Methods("GET").
		Path(conf.MetricsURLPath).
		Name(name).
		Handler(Logger(promhttp.Handler(), name))

	if c.debugLog != nil {
		c.debugLog.Printf("Collector configured\n")
	}

	return nil
}

func (c *Collector) Run(ctx context.Context) {
	log.Printf("Sample processing loop: starting")
	go c.processSamples()

	log.Printf("Enabling data sources...")
	for _, src := range c.srcs {
		go src.Run(ctx)
	}

	log.Printf("Prometheus endpoint: starting")
	log.Fatal(http.ListenAndServe(c.address, c.router))
}

func (c Collector) processSamples() {
	ticker := time.NewTicker(time.Minute).C
	for {
		if c.debugLog != nil {
			c.debugLog.Printf("Processing samples")
		}

		select {
		case vl := <-c.ch:
			c.update(vl)

		case <-ticker:
			c.purge(time.Now())
		}
	}
}

func (c Collector) update(vl api.ValueList) {
	id := vl.Identifier.String()
	if c.debugLog != nil {
		c.debugLog.Printf("Updating: %s", id)
	}
	c.rw.Lock()
	c.values[id] = vl
	c.rw.Unlock()
}

func (c Collector) purge(now time.Time) {
	c.rw.Lock()
	for id, vl := range c.values {
		expiration := vl.Time.Add(vl.Interval)
		if expiration.Before(now) {
			if c.debugLog != nil {
				c.debugLog.Printf("Purging: %s", id)
			}
			delete(c.values, id)
		}
	}
	c.rw.Unlock()
}

func (c Collector) Write(_ context.Context, vl *api.ValueList) error {
	if c.debugLog != nil {
		log.Printf("Writing: %s", vl.Identifier.String())
	}
	c.ch <- *vl
	return nil
}
