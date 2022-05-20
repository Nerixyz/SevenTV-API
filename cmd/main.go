package main

import (
	"context"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/SevenTV/Common/mongo"
	"github.com/SevenTV/Common/redis"
	"github.com/SevenTV/Common/structures/v3/mutations"
	"github.com/SevenTV/Common/structures/v3/query"
	"github.com/bugsnag/panicwrap"
	"github.com/meilisearch/meilisearch-go"
	"github.com/seventv/api/internal/configure"
	"github.com/seventv/api/internal/global"
	"github.com/seventv/api/internal/gql"
	"github.com/seventv/api/internal/health"
	"github.com/seventv/api/internal/loaders"
	"github.com/seventv/api/internal/monitoring"
	"github.com/seventv/api/internal/rest"
	"github.com/seventv/api/internal/svc/prometheus"
	"github.com/seventv/api/internal/svc/rmq"
	"github.com/seventv/api/internal/svc/s3"
	"go.uber.org/zap"
)

var (
	Version = "development"
	Unix    = ""
	Time    = "unknown"
	User    = "unknown"
)

func init() {
	debug.SetGCPercent(2000)
	if i, err := strconv.Atoi(Unix); err == nil {
		Time = time.Unix(int64(i), 0).Format(time.RFC3339)
	}
}

func main() {
	config := configure.New()

	exitStatus, err := panicwrap.BasicWrap(func(s string) {
		zap.S().Errorw("panic detected",
			"panic", s,
		)
	})
	if err != nil {
		zap.S().Errorw("failed to setup panic handler",
			"error", err,
		)
		os.Exit(2)
	}

	if exitStatus >= 0 {
		os.Exit(exitStatus)
	}

	if !config.NoHeader {
		zap.S().Info("7TV API")
		zap.S().Infof("Version: %s", Version)
		zap.S().Infof("build.Time: %s", Time)
		zap.S().Infof("build.User: %s", User)
	}

	zap.S().Debugf("MaxProcs: %d", runtime.GOMAXPROCS(0))

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	gCtx, cancel := global.WithCancel(global.New(context.Background(), config))

	{
		gCtx.Inst().Redis, err = redis.Setup(gCtx, redis.SetupOptions{
			Username:   config.Redis.Username,
			Password:   config.Redis.Password,
			Database:   config.Redis.Database,
			Sentinel:   config.Redis.Sentinel,
			Addresses:  config.Redis.Addresses,
			MasterName: config.Redis.MasterName,
		})
		if err != nil {
			zap.S().Fatalw("failed to setup redis handler",
				"error", err,
			)
		}
	}

	{
		gCtx.Inst().Mongo, err = mongo.Setup(gCtx, mongo.SetupOptions{
			URI:    config.Mongo.URI,
			DB:     config.Mongo.DB,
			Direct: config.Mongo.Direct,
		})
		if err != nil {
			zap.S().Fatalw("failed to setup mongo handler",
				"error", err,
			)
		}
	}

	{
		gCtx.Inst().RMQ, err = rmq.New(gCtx, rmq.Options{
			URI: config.RMQ.URI,
		})
		if err != nil {
			zap.S().Warnw("failed to setup rmq handler",
				"error", err,
			)
		}
	}

	{
		gCtx.Inst().S3, err = s3.New(gCtx, s3.Options{
			Region:      config.S3.Region,
			Endpoint:    config.S3.Endpoint,
			AccessToken: config.S3.AccessToken,
			SecretKey:   config.S3.SecretKey,
		})
		if err != nil {
			zap.S().Warnw("failed to setup s3 handler",
				"error", err,
			)
		}
	}

	{
		gCtx.Inst().Prometheus = prometheus.New(prometheus.Options{
			Labels: config.Monitoring.Labels.ToPrometheus(),
		})
	}

	{
		gCtx.Inst().Loaders = loaders.New(gCtx)
	}

	{
		gCtx.Inst().MeilieSearch = meilisearch.NewClient(meilisearch.ClientConfig{
			Host:    "http://meilisearch.meilisearch.svc.cluster.local:7700",
			APIKey:  "",
			Timeout: time.Second * 5,
		})
	}

	{
		gCtx.Inst().Query = query.New(gCtx.Inst().Mongo, gCtx.Inst().Redis)
		gCtx.Inst().Mutate = mutations.New(gCtx.Inst().Mongo, gCtx.Inst().Redis)
	}

	wg := sync.WaitGroup{}

	if gCtx.Config().Health.Enabled {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-health.New(gCtx)
		}()
	}
	if gCtx.Config().Monitoring.Enabled {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-monitoring.New(gCtx)
		}()
	}

	done := make(chan struct{})
	go func() {
		<-sig
		cancel()
		go func() {
			select {
			case <-time.After(time.Minute):
			case <-sig:
			}
			zap.S().Fatal("force shutdown")
		}()

		zap.S().Info("shutting down")

		wg.Wait()

		close(done)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := rest.New(gCtx); err != nil {
			zap.S().Fatalw("rest failed",
				"error", err,
			)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := gql.New(gCtx); err != nil {
			zap.S().Fatalw("gql failed",
				"error", err,
			)
		}
	}()

	zap.S().Info("running")

	<-done

	zap.S().Info("shutdown")
	os.Exit(0)
}
