// MIT License

// Copyright (c) [2022] [Bohdan Ivashko (https://github.com/Arriven)]

// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:

// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.

// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package main

import (
	"context"
	"flag"
	"math/rand"
	"net/http"
	pprofhttp "net/http/pprof"
	"os"
	"runtime"
	"strings"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/Arriven/db1000n/src/job"
	"github.com/Arriven/db1000n/src/job/config"
	"github.com/Arriven/db1000n/src/utils"
	"github.com/Arriven/db1000n/src/utils/metrics"
	"github.com/Arriven/db1000n/src/utils/ota"
)

const simpleLogFormat = "simple"

func main() {
	runnerConfigOptions := job.NewConfigOptionsWithFlags()
	jobsGlobalConfig := job.NewGlobalConfigWithFlags()
	otaConfig := ota.NewConfigWithFlags()
	countryCheckerConfig := utils.NewCountryCheckerConfigWithFlags()
	updaterMode, destinationPath := config.NewUpdaterOptionsWithFlags()
	prometheusOn, prometheusListenAddress := metrics.NewOptionsWithFlags()
	pprof := flag.String("pprof", utils.GetEnvStringDefault("GO_PPROF_ENDPOINT", ""), "enable pprof")
	help := flag.Bool("h", false, "print help message and exit")
	version := flag.Bool("version", false, "print version and exit")
	debug := flag.Bool("debug", utils.GetEnvBoolDefault("DEBUG", false), "enable debug level logging and features")
	logLevel := flag.String("log-level", utils.GetEnvStringDefault("LOG_LEVEL", "none"), "log level override for zap, leave empty to use default")
	logFormat := flag.String("log-format", utils.GetEnvStringDefault("LOG_FORMAT", simpleLogFormat), "overrides the default (simple) log output format,\n"+
		"possible values are: json, console, simple\n"+
		"simple is the most human readable format if you only look at the output in your terminal")
	lessStats := flag.Bool("less-stats", utils.GetEnvBoolDefault("LESS_STATS", false), "group target stats by protocols - in case you have too many targets")
	periodicGCEnabled := flag.Bool("periodic-gc", utils.GetEnvBoolDefault("PERIODIC_GC", false),
		"set to true if you want to run periodic garbage collection(useful in pooling scenarios, like db1000nx100)")

	flag.Parse()

	logger, err := newZapLogger(*debug, *logLevel, *logFormat)
	if err != nil {
		panic(err)
	}

	logger.Info("running db1000n", zap.String("version", ota.Version), zap.Int("pid", os.Getpid()))

	switch {
	case *help:
		flag.CommandLine.Usage()

		return
	case *version:
		return
	case *updaterMode:
		config.UpdateLocal(logger, *destinationPath, strings.Split(runnerConfigOptions.PathsCSV, ","), []byte(runnerConfigOptions.BackupConfig),
			jobsGlobalConfig.SkipEncrypted)

		return
	}

	err = utils.UpdateRLimit()
	if err != nil {
		logger.Warn("failed to increase rlimit", zap.Error(err))
	}

	go periodicGC(periodicGCEnabled, runnerConfigOptions.RefreshTimeout, logger)
	go ota.WatchUpdates(logger, otaConfig)
	setUpPprof(logger, *pprof, *debug)
	rand.Seed(time.Now().UnixNano())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	metrics.InitOrFail(ctx, logger, *prometheusOn, *prometheusListenAddress, jobsGlobalConfig.ClientID,
		utils.CheckCountryOrFail(ctx, logger, countryCheckerConfig, jobsGlobalConfig.GetProxyParams(logger, nil)))
	job.NewRunner(runnerConfigOptions, jobsGlobalConfig, newReporter(*logFormat, *lessStats, logger)).Run(ctx, logger)
}

func periodicGC(enabled *bool, period time.Duration, log *zap.Logger) {
	if !*enabled {
		return
	}

	var m runtime.MemStats

	for {
		<-time.After(period)
		runtime.ReadMemStats(&m)

		memBefore := m.Alloc
		start := time.Now()

		runtime.GC()
		runtime.ReadMemStats(&m)
		log.Info("GC finished",
			zap.Duration("GC took(Sec)", time.Since(start)),
			zap.Uint64("previous(MiB)", utils.ToMiB(memBefore)),
			zap.Uint64("current(MiB)", utils.ToMiB(m.Alloc)),
			zap.Uint64("recovered(MiB)", utils.ToMiB(memBefore-m.Alloc)),
		)
	}
}

func newZapLogger(debug bool, logLevel string, logFormat string) (*zap.Logger, error) {
	cfg := zap.NewProductionConfig()
	if debug {
		cfg = zap.NewDevelopmentConfig()
	}

	if logFormat == simpleLogFormat {
		// turn off all output except the message itself and log level
		cfg.Encoding = "console"
		cfg.EncoderConfig.TimeKey = ""
		cfg.EncoderConfig.NameKey = ""

		// turning these off for debug output would be undesirable
		if !debug {
			cfg.EncoderConfig.CallerKey = ""
			cfg.EncoderConfig.StacktraceKey = ""
		}
	} else if logFormat != "" {
		cfg.Encoding = logFormat

		if logFormat == "console" {
			cfg.EncoderConfig.EncodeTime = zapcore.RFC3339TimeEncoder
		}
	}

	level, err := zap.ParseAtomicLevel(logLevel)
	if err == nil {
		cfg.Level = level
	}

	return cfg.Build()
}

func setUpPprof(logger *zap.Logger, pprof string, debug bool) {
	switch {
	case debug && pprof == "":
		pprof = ":8080"
	case pprof == "":
		return
	}

	mux := http.NewServeMux()
	mux.Handle("/debug/pprof/", http.HandlerFunc(pprofhttp.Index))
	mux.Handle("/debug/pprof/cmdline", http.HandlerFunc(pprofhttp.Cmdline))
	mux.Handle("/debug/pprof/profile", http.HandlerFunc(pprofhttp.Profile))
	mux.Handle("/debug/pprof/symbol", http.HandlerFunc(pprofhttp.Symbol))
	mux.Handle("/debug/pprof/trace", http.HandlerFunc(pprofhttp.Trace))

	server := &http.Server{
		Addr:         pprof,
		Handler:      mux,
		ReadTimeout:  time.Second,
		WriteTimeout: time.Second,
	}

	// this has to be wrapped into a lambda bc otherwise it blocks when evaluating argument for zap.Error
	go func() { logger.Warn("pprof server", zap.Error(server.ListenAndServe())) }()
}

func newReporter(logFormat string, groupTargets bool, logger *zap.Logger) metrics.Reporter {
	if logFormat == simpleLogFormat {
		return metrics.NewConsoleReporter(os.Stdout, groupTargets)
	}

	return metrics.NewZapReporter(logger, groupTargets)
}
