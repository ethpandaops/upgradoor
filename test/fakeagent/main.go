// Command fakeagent runs agent on agenttest's in-memory host described by a
// scenario file, for local development: nothing on the machine is touched.
//
//	go run ./test/fakeagent --config agent.yaml --scenario host.yaml [--control 127.0.0.1:7771]
//
// --control serves an endpoint that changes the host while the agent runs.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/agent"
	"github.com/ethpandaops/upgradoor/pkg/agent/agenttest"
	"github.com/ethpandaops/upgradoor/pkg/identity"
)

func main() {
	var configFile, scenarioFile, control, logLevel string

	flag.StringVar(&configFile, "config", agent.DefaultConfigFile, "agent configuration file")
	flag.StringVar(&scenarioFile, "scenario", "", "scenario file of the in-memory host (required)")
	flag.StringVar(&control, "control", "", "loopback address of the control endpoint (optional)")
	flag.StringVar(&logLevel, "log-level", "info", "log level")
	flag.Parse()

	log := logrus.New()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, log, configFile, scenarioFile, control, logLevel)

	stop()

	if err != nil {
		log.WithError(err).Error("fakeagent failed")
		os.Exit(1)
	}
}

func run(ctx context.Context, log *logrus.Logger, configFile, scenarioFile, control, logLevel string) error {
	level, err := logrus.ParseLevel(logLevel)
	if err != nil {
		return err
	}

	log.SetLevel(level)

	if scenarioFile == "" {
		return errors.New("--scenario is required")
	}

	cfg, err := agent.LoadConfig(configFile)
	if err != nil {
		return err
	}

	creds, err := loadCredentials(cfg.KeyFile, cfg.CertFile)
	if err != nil {
		return err
	}

	sc, err := agenttest.LoadScenario(scenarioFile)
	if err != nil {
		return err
	}

	host, err := agenttest.NewHost(sc, &cfg.Host.Validators)
	if err != nil {
		return err
	}

	svc, err := agent.New(log, cfg, creds, &agent.Deps{
		Host: host, Resolver: host, Version: "fake",
	})
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup

	if control != "" {
		srv, cerr := newControl(host, control)
		if cerr != nil {
			return cerr
		}

		wg.Add(1)

		go func() {
			defer wg.Done()

			if serr := srv.serve(ctx); serr != nil {
				log.WithError(serr).Error("control endpoint")
			}
		}()
	}

	if err = svc.Start(ctx); err != nil {
		cancel()
		wg.Wait()

		return fmt.Errorf("start agent: %w", err)
	}

	<-ctx.Done()

	err = svc.Stop()

	cancel()
	wg.Wait()

	return err
}

// loadCredentials reads the host key and certificate.
func loadCredentials(keyFile, certFile string) (*identity.Credentials, error) {
	rawKey, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, fmt.Errorf("read host key: %w", err)
	}

	key, err := identity.DecodePrivateKey(string(rawKey))
	if err != nil {
		return nil, fmt.Errorf("host key %s: %w", keyFile, err)
	}

	rawCert, err := os.ReadFile(certFile)
	if err != nil {
		return nil, fmt.Errorf("read host certificate: %w", err)
	}

	creds, err := identity.NewCredentials(key, string(bytes.TrimSpace(rawCert)))
	if err != nil {
		return nil, fmt.Errorf("host certificate %s: %w", certFile, err)
	}

	return creds, nil
}
