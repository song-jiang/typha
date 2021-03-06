// Copyright (c) 2017 Tigera, Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package daemon

import (
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"runtime/pprof"
	"strings"
	"syscall"
	"time"

	docopt "github.com/docopt/docopt-go"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	log "github.com/sirupsen/logrus"

	"github.com/projectcalico/libcalico-go/lib/api"
	"github.com/projectcalico/libcalico-go/lib/backend"
	bapi "github.com/projectcalico/libcalico-go/lib/backend/api"
	"github.com/projectcalico/libcalico-go/lib/health"
	"github.com/projectcalico/typha/pkg/buildinfo"
	"github.com/projectcalico/typha/pkg/calc"
	"github.com/projectcalico/typha/pkg/config"
	"github.com/projectcalico/typha/pkg/jitter"
	"github.com/projectcalico/typha/pkg/k8s"
	"github.com/projectcalico/typha/pkg/logutils"
	"github.com/projectcalico/typha/pkg/snapcache"
	"github.com/projectcalico/typha/pkg/syncserver"
)

const usage = `Typha, Calico's fan-out proxy.

Usage:
  calico-typha [options]

Options:
  -c --config-file=<filename>  Config file to load [default: /etc/calico/typha.cfg].
  --version                    Print the version and exit.
`

// TyphaDaemon handles the lifecycle of the Typha process.  The main() function of the Typha executable
// should simply call InitializeAndServeForever() to start the Typha server.  The lifecycle is broken out into
// several individual methods for ease of testing.
type TyphaDaemon struct {
	BuildInfoLogCxt *log.Entry
	ConfigFilePath  string
	DatastoreClient BackendClient
	ConfigParams    *config.Config

	// The components of the server, created in CreateServer() below.
	Syncer            bapi.Syncer
	SyncerToValidator *calc.SyncerCallbacksDecoupler
	Validator         *calc.ValidationFilter
	ValidatorToCache  *calc.SyncerCallbacksDecoupler
	Cache             *snapcache.Cache
	Server            *syncserver.Server

	// The functions below default to real library functions but they can be overridden for testing.
	NewBackendClient      func(config api.CalicoAPIConfig) (BackendClient, error)
	ConfigureEarlyLogging func()
	ConfigureLogging      func(configParams *config.Config)

	// Health monitoring.
	healthAggregator *health.HealthAggregator
}

func New() *TyphaDaemon {
	return &TyphaDaemon{
		NewBackendClient: func(config api.CalicoAPIConfig) (BackendClient, error) {
			return backend.NewClient(config)
		},
		ConfigureEarlyLogging: logutils.ConfigureEarlyLogging,
		ConfigureLogging:      logutils.ConfigureLogging,
	}
}

func (t *TyphaDaemon) InitializeAndServeForever(cxt context.Context) {
	t.DoEarlyRuntimeSetup()
	t.ParseCommandLineArgs(nil)
	t.LoadConfiguration()
	t.CreateServer()
	t.Start(cxt)
	t.WaitAndShutDown(cxt)
}

// DoEarlyRuntimeSetup does early runtime/logging configuration that needs to happen before we do any work.
func (t *TyphaDaemon) DoEarlyRuntimeSetup() {
	// Go's RNG is not seeded by default.  Do that now.
	rand.Seed(time.Now().UTC().UnixNano())

	// Special-case handling for environment variable-configured logging:
	// Initialise early so we can trace out config parsing.
	t.ConfigureEarlyLogging()
}

// ParseCommandLineArgs parses the command line args and either exits with a usage warning or stores the parsed
// arguments on fields of the struct.
func (t *TyphaDaemon) ParseCommandLineArgs(argv []string) {
	// Parse command-line args.
	version := "Version:            " + buildinfo.GitVersion + "\n" +
		"Full git commit ID: " + buildinfo.GitRevision + "\n" +
		"Build date:         " + buildinfo.BuildDate + "\n"
	arguments, err := docopt.Parse(usage, argv, true, version, false)
	if err != nil {
		println(usage)
		log.Fatalf("Failed to parse usage, exiting: %v", err)
	}
	t.ConfigFilePath = arguments["--config-file"].(string)
	t.BuildInfoLogCxt = log.WithFields(log.Fields{
		"version":    buildinfo.GitVersion,
		"buildDate":  buildinfo.BuildDate,
		"gitCommit":  buildinfo.GitRevision,
		"GOMAXPROCS": runtime.GOMAXPROCS(0),
	})
	t.BuildInfoLogCxt.Info("Typha starting up")
	log.Infof("Command line arguments: %v", arguments)
}

// LoadConfiguration uses the command-line configuration and environment variables to load our configuration.
// It initializes the datastore connection.
func (t *TyphaDaemon) LoadConfiguration() {
	// Load the configuration from all the different sources including the
	// datastore and merge. Keep retrying on failure.  We'll sit in this
	// loop until the datastore is ready.
	log.Infof("Loading configuration...")
	var configParams *config.Config
configRetry:
	for {
		// Load locally-defined config, including the datastore connection
		// parameters. First the environment variables.
		configParams = config.New()
		envConfig := config.LoadConfigFromEnvironment(os.Environ())
		// Then, the config file.
		fileConfig, err := config.LoadConfigFile(t.ConfigFilePath)
		if err != nil {
			log.WithError(err).WithField("configFile", t.ConfigFilePath).Error(
				"Failed to load configuration file")
			time.Sleep(1 * time.Second)
			continue configRetry
		}
		// Parse and merge the local config.
		_, err = configParams.UpdateFrom(envConfig, config.EnvironmentVariable)
		if err != nil {
			log.WithError(err).WithField("configFile", t.ConfigFilePath).Error(
				"Failed to parse configuration environment variable")
			time.Sleep(1 * time.Second)
			continue configRetry
		}
		_, err = configParams.UpdateFrom(fileConfig, config.ConfigFile)
		if err != nil {
			log.WithError(err).WithField("configFile", t.ConfigFilePath).Error(
				"Failed to parse configuration file")
			time.Sleep(1 * time.Second)
			continue configRetry
		}

		// We should now have enough config to connect to the datastore
		// so we can load the remainder of the config.
		datastoreConfig := configParams.DatastoreConfig()
		t.DatastoreClient, err = t.NewBackendClient(datastoreConfig)
		if err != nil {
			log.WithError(err).Error("Failed to connect to datastore")
			time.Sleep(1 * time.Second)
			continue configRetry
		}

		err = configParams.Validate()
		if err != nil {
			log.WithError(err).Error(
				"Failed to parse/validate configuration from datastore.")
			time.Sleep(1 * time.Second)
			continue configRetry
		}
		break configRetry
	}

	// If we get here, we've loaded the configuration successfully.
	// Update log levels before we do anything else.
	t.ConfigureLogging(configParams)
	// Since we may have enabled more logging, log with the build context
	// again.
	t.BuildInfoLogCxt.WithField("config", configParams).Info(
		"Successfully loaded configuration.")

	// Make sure the datastore is initialized, otherwise the Syncer may spin, looking for
	// non-existent resources.
	for {
		err := t.DatastoreClient.EnsureInitialized()
		if err != nil {
			log.WithError(err).Error("Failed to ensure datastore was initialized")
			time.Sleep(1 * time.Second)
			continue
		}
		break
	}
	t.ConfigParams = configParams
}

// CreateServer creates and configures (but does not start) the server components.
func (t *TyphaDaemon) CreateServer() {
	// Now create the Syncer; our caching layer and the TCP server.

	// Health monitoring, for liveness and readiness endpoints.
	t.healthAggregator = health.NewHealthAggregator()

	// Get a Syncer from the datastore, which will feed the validator layer with updates.
	t.SyncerToValidator = calc.NewSyncerCallbacksDecoupler()
	t.Syncer = t.DatastoreClient.Syncer(t.SyncerToValidator)
	log.Debugf("Created Syncer: %#v", t.Syncer)

	// Create the validator, which sits between the syncer and the cache.
	t.ValidatorToCache = calc.NewSyncerCallbacksDecoupler()
	t.Validator = calc.NewValidationFilter(t.ValidatorToCache)

	// Create our snapshot cache, which stores point-in-time copies of the datastore contents.
	t.Cache = snapcache.New(snapcache.Config{
		MaxBatchSize:     t.ConfigParams.SnapshotCacheMaxBatchSize,
		HealthAggregator: t.healthAggregator,
	})

	// Create the server, which listens for connections from Felix.
	t.Server = syncserver.New(
		t.Cache,
		syncserver.Config{
			MaxMessageSize:          t.ConfigParams.ServerMaxMessageSize,
			MinBatchingAgeThreshold: t.ConfigParams.ServerMinBatchingAgeThresholdSecs,
			MaxFallBehind:           t.ConfigParams.ServerMaxFallBehindSecs,
			PingInterval:            t.ConfigParams.ServerPingIntervalSecs,
			PongTimeout:             t.ConfigParams.ServerPongTimeoutSecs,
			DropInterval:            t.ConfigParams.ConnectionDropIntervalSecs,
			MaxConns:                t.ConfigParams.MaxConnectionsUpperLimit,
			Port:                    t.ConfigParams.ServerPort,
			HealthAggregator:        t.healthAggregator,
		},
	)
}

// Start starts all the server components in background goroutines.
func (t *TyphaDaemon) Start(cxt context.Context) {
	// Now we've connected everything up, start the background processing threads.
	log.Info("Starting the datastore Syncer/cache layer")
	t.Syncer.Start()
	go t.SyncerToValidator.SendToContext(cxt, t.Validator)
	go t.ValidatorToCache.SendToContext(cxt, t.Cache)
	t.Cache.Start(cxt)
	t.Server.Start(cxt)
	if t.ConfigParams.ConnectionRebalancingMode == "kubernetes" {
		log.Info("Kubernetes connection rebalancing is enabled, starting k8s poll goroutine.")
		k8sAPI := k8s.NewK8sAPI()
		ticker := jitter.NewTicker(
			t.ConfigParams.K8sServicePollIntervalSecs,
			t.ConfigParams.K8sServicePollIntervalSecs/10)
		go k8s.PollK8sForConnectionLimit(cxt, t.ConfigParams, ticker.C, k8sAPI, t.Server)
	}
	log.Info("Started the datastore Syncer/cache layer/server.")

	if t.ConfigParams.PrometheusMetricsEnabled {
		log.Info("Prometheus metrics enabled.  Starting server.")
		go servePrometheusMetrics(t.ConfigParams)
	}

	if t.ConfigParams.HealthEnabled {
		log.WithField("port", t.ConfigParams.HealthPort).Info("Health enabled.  Starting server.")
		go t.healthAggregator.ServeHTTP(t.ConfigParams.HealthPort)
	}
}

// WaitAndShutDown waits for OS signals or context.Done() and exits as appropriate.
func (t *TyphaDaemon) WaitAndShutDown(cxt context.Context) {
	// Hook and process the signals we care about
	usr1SignalChan := make(chan os.Signal, 1)
	signal.Notify(usr1SignalChan, syscall.SIGUSR1)
	termChan := make(chan os.Signal, 1)
	signal.Notify(termChan, syscall.SIGTERM)
	for {
		select {
		case <-termChan:
			log.Fatal("Received SIGTERM, shutting down")
		case <-usr1SignalChan:
			log.Info("Received SIGUSR1, emitting heap profile")
			dumpHeapMemoryProfile(t.ConfigParams)
		case <-cxt.Done():
			log.Info("Context asked us to stop.")
			return
		}
	}
}

// BackendClient captures the sub-interface of the backend client that we actually use.
type BackendClient interface {
	// Syncer creates an object that generates a series of KVPair updates,
	// which paint an eventually-consistent picture of the full state of
	// the datastore and then generates subsequent KVPair updates for
	// changes to the datastore.
	Syncer(callbacks bapi.SyncerCallbacks) bapi.Syncer

	// EnsureInitialized ensures that the backend is initialized
	// any ready to be used.
	EnsureInitialized() error
}

// TODO Typha: Share with Felix.
func dumpHeapMemoryProfile(configParams *config.Config) {
	// If a memory profile file name is configured, dump a heap memory profile.  If the
	// configured filename includes "<timestamp>", that will be replaced with a stamp indicating
	// the current time.
	memProfFileName := configParams.DebugMemoryProfilePath
	if memProfFileName != "" {
		logCxt := log.WithField("file", memProfFileName)
		logCxt.Info("Asked to create a memory profile.")

		// If the configured file name includes "<timestamp>", replace that with the current
		// time.
		if strings.Contains(memProfFileName, "<timestamp>") {
			timestamp := time.Now().Format("2006-01-02-15:04:05")
			memProfFileName = strings.Replace(memProfFileName, "<timestamp>", timestamp, 1)
			logCxt = log.WithField("file", memProfFileName)
		}

		// Open a file with that name.
		memProfFile, err := os.Create(memProfFileName)
		if err != nil {
			logCxt.WithError(err).Fatal("Could not create memory profile file")
			memProfFile = nil
		} else {
			defer func() {
				err := memProfFile.Close()
				if err != nil {
					log.WithError(err).Error("Error while closing memory profile file.")
				}
			}()
			logCxt.Info("Writing memory profile...")
			// The initial resync uses a lot of scratch space so now is
			// a good time to force a GC and return any RAM that we can.
			debug.FreeOSMemory()
			if err := pprof.WriteHeapProfile(memProfFile); err != nil {
				logCxt.WithError(err).Error("Could not write memory profile")
			}
			logCxt.Info("Finished writing memory profile")
		}
	}
}

// TODO Typha: Share with Felix.
func servePrometheusMetrics(configParams *config.Config) {
	for {
		log.WithField("port", configParams.PrometheusMetricsPort).Info("Starting prometheus metrics endpoint")
		if configParams.PrometheusGoMetricsEnabled && configParams.PrometheusProcessMetricsEnabled {
			log.Info("Including Golang & Process metrics")
		} else {
			if !configParams.PrometheusGoMetricsEnabled {
				log.Info("Discarding Golang metrics")
				prometheus.Unregister(prometheus.NewGoCollector())
			}
			if !configParams.PrometheusProcessMetricsEnabled {
				log.Info("Discarding process metrics")
				prometheus.Unregister(prometheus.NewProcessCollector(os.Getpid(), ""))
			}
		}
		http.Handle("/metrics", promhttp.Handler())
		err := http.ListenAndServe(fmt.Sprintf(":%v", configParams.PrometheusMetricsPort), nil)
		log.WithError(err).Error(
			"Prometheus metrics endpoint failed, trying to restart it...")
		time.Sleep(1 * time.Second)
	}
}
