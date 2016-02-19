package main

/*
 * Microservice gateway application
 * Copyright (C) 2015  Martin Helmich <m.helmich@mittwald.de>
 *                     Mittwald CM Service GmbH & Co. KG
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */

import (
	"encoding/json"
	"flag"
	"fmt"
	"github.com/garyburd/redigo/redis"
	"github.com/hashicorp/consul/api"
	"github.com/mailgun/manners"
	"github.com/mittwald/servicegateway/auth"
	"github.com/mittwald/servicegateway/cache"
	"github.com/mittwald/servicegateway/config"
	"github.com/mittwald/servicegateway/dispatcher"
	"github.com/mittwald/servicegateway/proxy"
	"github.com/mittwald/servicegateway/ratelimit"
	logging "github.com/op/go-logging"
	"io/ioutil"
	"os"
	"strings"
	"time"
	"github.com/mittwald/servicegateway/admin"
	"net/http"
	"runtime/pprof"
	"os/signal"
)

type StartupConfig struct {
	ConfigSource    string
	ConfigFile      string
	DispatchingMode string
	ConsulBaseKey   string
	UiDir           string
	Port            int
	AdminAddress    string
	AdminPort       int
	Debug           bool
	ProfileCpu      string
}

func main() {
	startup := StartupConfig{}

	flag.StringVar(&startup.ConfigFile, "config", "/etc/servicegateway.json", "configuration file")
	flag.StringVar(&startup.DispatchingMode, "dispatch", "path", "dispatching mode ('path' or 'host')")
	flag.IntVar(&startup.Port, "port", 8080, "HTTP port to listen on")
	flag.StringVar(&startup.AdminAddress, "admin-addr", "127.0.0.1", "Address to listen on (administration port)")
	flag.IntVar(&startup.AdminPort, "admin-port", 8081, "HTTP port to listen on (administration port)")
	flag.BoolVar(&startup.Debug, "debug", false, "enable to add debug information to each request")
	flag.StringVar(&startup.ConsulBaseKey, "consul-base", "gateway/ui", "base key name for configuration")
	flag.StringVar(&startup.UiDir, "ui-dir", "/usr/share/servicegateway", "directory in which UI files can be found")

	flag.StringVar(&startup.ProfileCpu, "cpu-profile", "", "write CPU profile to file")

	flag.Parse()

	logger := logging.MustGetLogger("startup")
	format := logging.MustStringFormatter("%{color}%{time:15:04:05.000} %{module:12s} ▶ %{level:.4s} %{id:03x}%{color:reset} %{message}")
	backend := logging.NewLogBackend(os.Stderr, "", 0)

	if startup.ProfileCpu != "" {
        f, err := os.Create(startup.ProfileCpu)
        if err != nil {
            logger.Fatal(err)
        }
        pprof.StartCPUProfile(f)
        defer pprof.StopCPUProfile()

		c := make(chan os.Signal, 1)
		signal.Notify(c, os.Interrupt)
		go func(){
			for _ = range c {
				pprof.StopCPUProfile()
				os.Exit(0)
			}
		}()
    }

	logging.SetBackend(logging.NewBackendFormatter(backend, format))
	logger.Info("Completed startup")

	cfg := config.Configuration{}
	data, err := ioutil.ReadFile(startup.ConfigFile)
	if err != nil {
		logger.Fatal(err)
	}

	err = json.Unmarshal(data, &cfg)
	if err != nil {
		logger.Fatal(err)
		panic(err)
	}

	logger.Debugf("%s", cfg)

	consulConfig := api.DefaultConfig()
	consulConfig.Address = cfg.Consul.Address()
	consulConfig.Datacenter = cfg.Consul.DataCenter

	consulClient, err := api.NewClient(consulConfig)
	if err != nil {
		logger.Panic(err)
	}

	redisPool := redis.NewPool(func() (redis.Conn, error) {
		return redis.Dial("tcp", cfg.Redis)
	}, 8)

	tokenVerifier, err := auth.NewJwtVerifier(&cfg.Authentication)
	if err != nil {
		logger.Panic(err)
	}

	tokenStore, err := auth.NewTokenStore(redisPool, tokenVerifier, auth.TokenStoreOptions{})
	if err != nil {
		logger.Panic(err)
	}

	handler := proxy.NewProxyHandler(logging.MustGetLogger("proxy"), &cfg)

	listenAddress := fmt.Sprintf(":%d", startup.Port)
	adminListenAddress := fmt.Sprintf("%s:%d", startup.AdminAddress, startup.AdminPort)

	done := make(chan bool)

	go func() {
		var lastIndex uint64 = 0
		var newLastIndex uint64 = 0
		var err error

		dispChan := make(chan dispatcher.Dispatcher)
		admChan := make(chan http.Handler)

		go func() {
			for disp := range dispChan {
				logger.Infof("starting dispatcher on address %s", listenAddress)
				manners.ListenAndServe(listenAddress, disp)
			}
		}()

		go func() {
			for handler := range admChan {
				logger.Infof("starting admin server on address %s", adminListenAddress)
				manners.ListenAndServe(adminListenAddress, handler)
			}
		}()

		for {
			var dispatcher dispatcher.Dispatcher
			var adminServer http.Handler

			if lastIndex > 0 {
				time.Sleep(30 * time.Second)
			}

			dispatcher, adminServer, newLastIndex, err = buildDispatcher(
				&startup,
				&cfg,
				consulClient,
				handler,
				redisPool,
				logger,
				tokenStore,
				tokenVerifier,
				lastIndex,
			)

			if err != nil {
				logger.Error(err.Error())
				if lastIndex == 0 {
					logger.Panic("error on startup")
				}
			} else {
				lastIndex = newLastIndex

				manners.Close()
				dispChan <- dispatcher
				admChan <- adminServer
			}
		}
	}()

	logger.Info("waiting to die")
	<-done
}

func buildDispatcher(
	startup *StartupConfig,
	cfg *config.Configuration,
	consul *api.Client,
	handler *proxy.ProxyHandler,
	rpool *redis.Pool,
	logger *logging.Logger,
	tokenStore auth.TokenStore,
	tokenVerifier *auth.JwtVerifier,
	lastIndex uint64,
) (dispatcher.Dispatcher, http.Handler, uint64, error) {
	var disp dispatcher.Dispatcher
	var err error
	var meta *api.QueryMeta
	var configs api.KVPairs
	var localCfg config.Configuration = *cfg
	var appCfgs map[string]config.Application = make(map[string]config.Application)

	dispLogger := logging.MustGetLogger("dispatch")

	switch startup.DispatchingMode {
	case "path":
		disp, err = dispatcher.NewPathBasedDispatcher(&localCfg, dispLogger, handler)
//	case "host":
//		disp, err = dispatcher.NewHostBasedDispatcher(&localCfg, dispLogger, handler)
	default:
		err = fmt.Errorf("unsupported dispatching mode: '%s'", startup.DispatchingMode)
	}

	if err != nil {
		return nil, nil, 0, fmt.Errorf("error while creating proxy builder: %s", err)
	}

	applicationConfigBase := startup.ConsulBaseKey + "/applications"
	queryOpts := api.QueryOptions{
		WaitIndex: lastIndex,
		WaitTime:  30 * time.Minute,
	}

	logger.Infof("loading gateway config from KV %s", startup.ConsulBaseKey)
	configs, meta, err = consul.KV().List(startup.ConsulBaseKey, &queryOpts)
	if err != nil {
		return nil, nil, 0, err
	}

	for _, cfgKVPair := range configs {
		logger.Debugf("found KV pair with key '%s'", cfgKVPair.Key)

		switch strings.TrimPrefix(startup.ConsulBaseKey+"/", cfgKVPair.Key) {
		case "rate_limiting":
			if err := json.Unmarshal(cfgKVPair.Value, &localCfg.RateLimiting); err != nil {
				return nil, nil, meta.LastIndex, fmt.Errorf("JSON error on consul KV pair '%s': %s", cfgKVPair.Key, err)
			}
		}

		if strings.HasPrefix(cfgKVPair.Key, applicationConfigBase) {
			var appCfg config.Application

			if err := json.Unmarshal(cfgKVPair.Value, &appCfg); err != nil {
				return nil, nil, meta.LastIndex, fmt.Errorf("JSON error on consul KV pair '%s': %s", cfgKVPair.Key, err)
			}

			name := strings.TrimPrefix(cfgKVPair.Key, applicationConfigBase+"/")
			appCfgs[name] = appCfg
		}
	}

	authHandler, err := auth.NewAuthenticationHandler(&localCfg.Authentication, rpool, tokenStore, tokenVerifier, logger)
	if err != nil {
		return nil, nil, meta.LastIndex, err
	}

	authDecorator, err := auth.NewAuthDecorator(&localCfg.Authentication, rpool, logging.MustGetLogger("auth"), authHandler, tokenStore, startup.UiDir)
	if err != nil {
		return nil, nil, meta.LastIndex, err
	}

	rlim, err := ratelimit.NewRateLimiter(localCfg.RateLimiting, rpool, logging.MustGetLogger("ratelimiter"))
	if err != nil {
		logger.Fatalf("error while configuring rate limiting: %s", err)
	}

	cch := cache.NewCache(4096)

	// Order is important here! Behaviours will be called in LIFO order;
	// behaviours that are added last will be called first!
	disp.AddBehaviour(dispatcher.NewCachingBehaviour(cch))
	disp.AddBehaviour(dispatcher.NewAuthenticationBehaviour(authDecorator))
	disp.AddBehaviour(dispatcher.NewRatelimitBehaviour(rlim))

	for name, appCfg := range appCfgs {
		logger.Infof("registering application '%s' from Consul", name)
		if err := disp.RegisterApplication(name, appCfg); err != nil {
			return nil, nil, meta.LastIndex, err
		}
	}

	for name, appCfg := range localCfg.Applications {
		logger.Infof("registering application '%s' from local config", name)
		if err := disp.RegisterApplication(name, appCfg); err != nil {
			return nil, nil, meta.LastIndex, err
		}
	}

	if err = disp.Initialize(); err != nil {
		return nil, nil, meta.LastIndex, err
	}

	adminLogger, err := logging.GetLogger("admin-api")
	if err != nil {
		return nil, nil, meta.LastIndex, err
	}

	admin, err := admin.NewAdminServer(tokenStore, tokenVerifier, authHandler, adminLogger)
	if err != nil {
		return nil, nil, meta.LastIndex, err
	}

	return disp, admin, meta.LastIndex, nil
}
