package e2e_test

import (
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/improbable-eng/thanos/pkg/testutil"

	"path"

	"encoding/json"

	"github.com/oklog/run"
	"github.com/pkg/errors"
)

var (
	promHTTPPort = func(i int) string { return fmt.Sprintf("%d", 9090+i) }
	// We keep this one with localhost, to have perfect match with what Prometheus will expose in up metric.
	promHTTP = func(i int) string { return fmt.Sprintf("localhost:%s", promHTTPPort(i)) }

	sidecarGRPC    = func(i int) string { return fmt.Sprintf("127.0.0.1:%d", 19090+i) }
	sidecarHTTP    = func(i int) string { return fmt.Sprintf("127.0.0.1:%d", 19190+i) }
	sidecarCluster = func(i int) string { return fmt.Sprintf("127.0.0.1:%d", 19390+i) }

	queryGRPC    = func(i int) string { return fmt.Sprintf("127.0.0.1:%d", 19490+i) }
	queryHTTP    = func(i int) string { return fmt.Sprintf("127.0.0.1:%d", 19590+i) }
	queryCluster = func(i int) string { return fmt.Sprintf("127.0.0.1:%d", 19690+i) }

	rulerGRPC    = func(i int) string { return fmt.Sprintf("127.0.0.1:%d", 19790+i) }
	rulerHTTP    = func(i int) string { return fmt.Sprintf("127.0.0.1:%d", 19890+i) }
	rulerCluster = func(i int) string { return fmt.Sprintf("127.0.0.1:%d", 19990+i) }
)

type config struct {
	// Each config is for each Prometheus.
	promConfigs []string
	rules       string
	workDir     string

	numQueries          int
	queriesReplicaLabel string
	numRules            int
	numAlertmanagers    int
	sdConfig            sdConfig
}

type sdConfig struct {
	useGossip           bool
	useStaticStoresFlag bool
	useFileSD           bool
}

func evalClusterPeersFlags(cfg config) []string {
	var flags []string
	for i := 1; i <= len(cfg.promConfigs); i++ {
		flags = append(flags, "--cluster.peers", sidecarCluster(i))
	}
	for i := 1; i <= cfg.numQueries; i++ {
		flags = append(flags, "--cluster.peers", queryCluster(i))
	}
	for i := 1; i <= cfg.numRules; i++ {
		flags = append(flags, "--cluster.peers", rulerCluster(i))
	}
	return flags
}

// NOTE: It is important to install Thanos before using this function to compile latest changes.
func spinup(t testing.TB, ctx context.Context, cfg config) (chan error, error) {
	var (
		commands     []*exec.Cmd
		clusterPeers = evalClusterPeersFlags(cfg)
	)

	for k, promConfig := range cfg.promConfigs {
		i := k + 1
		promDir := fmt.Sprintf("%s/data/prom%d", cfg.workDir, i)

		if err := os.MkdirAll(promDir, 0777); err != nil {
			return nil, errors.Wrap(err, "create prom dir failed")
		}
		err := ioutil.WriteFile(promDir+"/prometheus.yml", []byte(promConfig), 0666)
		if err != nil {
			return nil, errors.Wrap(err, "creating prom config failed")
		}

		commands = append(commands, exec.Command(testutil.PrometheusBinary(),
			"--config.file", promDir+"/prometheus.yml",
			"--storage.tsdb.path", promDir,
			"--log.level", "info",
			"--web.listen-address", promHTTP(i),
		))

		args := []string{
			"sidecar",
			"--debug.name", fmt.Sprintf("sidecar-%d", i),
			"--grpc-address", sidecarGRPC(i),
			"--http-address", sidecarHTTP(i),
			"--prometheus.url", fmt.Sprintf("http://%s", promHTTP(i)),
			"--tsdb.path", promDir,
			"--log.level", "debug",
		}
		args = append(args, []string{
			"--cluster.address", sidecarCluster(i),
			"--cluster.advertise-address", sidecarCluster(i),
			"--cluster.gossip-interval", "200ms",
			"--cluster.pushpull-interval", "200ms"}...)
		args = append(args, clusterPeers...)

		commands = append(commands, exec.Command("thanos", args...))

		time.Sleep(200 * time.Millisecond)
	}

	for i := 1; i <= cfg.numQueries; i++ {
		args := []string{
			"query",
			"--debug.name", fmt.Sprintf("query-%d", i),
			"--grpc-address", queryGRPC(i),
			"--http-address", queryHTTP(i),
			"--log.level", "debug",
			"--query.replica-label", cfg.queriesReplicaLabel,
		}

		if cfg.sdConfig.useGossip {
			args = append(args, []string{
				"--cluster.address", queryCluster(i),
				"--cluster.advertise-address", queryCluster(i),
				"--cluster.gossip-interval", "200ms",
				"--cluster.pushpull-interval", "200ms",
			}...)
			args = append(args, clusterPeers...)
		} else {
			args = append(args, "--no-gossip")
		}

		if cfg.sdConfig.useStaticStoresFlag {
			for k := range cfg.promConfigs {
				args = append(args, "--store", sidecarGRPC(k+1))
			}
			for k := 0; k < cfg.numRules; k++ {
				args = append(args, "--store", rulerGRPC(k+1))
			}
		}

		if cfg.sdConfig.useFileSD {
			queryFileSDDir := fmt.Sprintf("%s/data/queryFileSd%d", cfg.workDir, i)
			if err := os.MkdirAll(queryFileSDDir, 0777); err != nil {
				return nil, errors.Wrap(err, "creating query filesd dir failed")
			}
			addrs := []string{}
			for k := range cfg.promConfigs {
				addrs = append(addrs, sidecarGRPC(k+1))
			}
			for k := 0; k < cfg.numRules; k++ {
				addrs = append(addrs, rulerGRPC(k+1))
			}
			json, err := json.Marshal(addrs)
			if err != nil {
				return nil, errors.Wrap(err, "encoding filesd failed")
			}
			err = ioutil.WriteFile(queryFileSDDir+"/filesd.json", json, 0666)
			if err != nil {
				return nil, errors.Wrap(err, "creating ruler file failed")
			}

			args = append(args, "--filesd", path.Join(queryFileSDDir, "filesd.json"))
		}

		commands = append(commands, exec.Command("thanos", args...))
		time.Sleep(200 * time.Millisecond)
	}

	for i := 1; i <= cfg.numRules; i++ {
		dbDir := fmt.Sprintf("%s/data/rule%d", cfg.workDir, i)

		if err := os.MkdirAll(dbDir, 0777); err != nil {
			return nil, errors.Wrap(err, "creating ruler dir failed")
		}
		err := ioutil.WriteFile(dbDir+"/rules.yaml", []byte(cfg.rules), 0666)
		if err != nil {
			return nil, errors.Wrap(err, "creating ruler file failed")
		}

		args := []string{
			"rule",
			"--debug.name", fmt.Sprintf("rule-%d", i),
			"--label", fmt.Sprintf(`replica="%d"`, i),
			"--data-dir", dbDir,
			"--rule-file", path.Join(dbDir, "*.yaml"),
			"--eval-interval", "1s",
			"--alertmanagers.url", "http://127.0.0.1:29093",
			"--grpc-address", rulerGRPC(i),
			"--http-address", rulerHTTP(i),
			"--log.level", "debug",
		}
		if cfg.sdConfig.useGossip {
			args = append(args, []string{
				"--cluster.address", rulerCluster(i),
				"--cluster.advertise-address", rulerCluster(i),
				"--cluster.gossip-interval", "200ms",
				"--cluster.pushpull-interval", "200ms",
			}...)
			args = append(args, clusterPeers...)
		} else {
			args = append(args, "--no-gossip")
		}

		if cfg.sdConfig.useFileSD {
			ruleFileSDDir := fmt.Sprintf("%s/data/ruleFileSd%d", cfg.workDir, i)
			if err := os.MkdirAll(ruleFileSDDir, 0777); err != nil {
				return nil, errors.Wrap(err, "creating rule filesd dir failed")
			}
			addrs := []string{}
			for k := 0; k < cfg.numQueries; k++ {
				addrs = append(addrs, queryHTTP(k+1))
			}
			json, err := json.Marshal(addrs)
			if err != nil {
				return nil, errors.Wrap(err, "encoding filesd failed")
			}
			err = ioutil.WriteFile(ruleFileSDDir+"/filesd.json", json, 0666)
			if err != nil {
				return nil, errors.Wrap(err, "creating ruler file failed")
			}

			args = append(args, "--filesd", path.Join(ruleFileSDDir, "filesd.json"))
		}

		if cfg.sdConfig.useStaticStoresFlag {
			for k := 0; k < cfg.numQueries; k++ {
				args = append(args, "--query", queryHTTP(k+1))
			}
		}

		commands = append(commands, exec.Command("thanos", args...))

		time.Sleep(200 * time.Millisecond)
	}

	for i := 1; i <= cfg.numAlertmanagers; i++ {
		dir := fmt.Sprintf("%s/data/alertmanager%d", cfg.workDir, i)

		if err := os.MkdirAll(dir, 0777); err != nil {
			return nil, errors.Wrap(err, "creating alertmanager dir failed")
		}
		config := `
route:
  group_by: ['alertname']
  group_wait: 1s
  group_interval: 1s
  receiver: 'null'
receivers:
- name: 'null'
`
		err := ioutil.WriteFile(dir+"/config.yaml", []byte(config), 0666)
		if err != nil {
			return nil, errors.Wrap(err, "creating alertmanager config file failed")
		}
		commands = append(commands, exec.Command(testutil.AlertmanagerBinary(),
			"--config.file", dir+"/config.yaml",
			"--web.listen-address", "127.0.0.1:29093",
			"--log.level", "debug",
		))
	}

	var g run.Group

	// Interrupt go routine.
	{
		ctx, cancel := context.WithCancel(ctx)
		g.Add(func() error {
			<-ctx.Done()

			// This go routine will return only when:
			// 1) Any other process from group exited unexpectedly
			// 2) Global context will be cancelled.
			return nil
		}, func(error) {
			cancel()
		})
	}

	// Run go routine for each command.
	for _, c := range commands {
		var stderr, stdout bytes.Buffer
		c.Stderr = &stderr
		c.Stdout = &stdout

		err := c.Start()
		if err != nil {
			// Let already started commands finish.
			go func() { _ = g.Run() }()
			return nil, errors.Wrap(err, "failed to start")
		}

		cmd := c
		g.Add(func() error {
			err := cmd.Wait()

			if stderr.Len() > 0 {
				t.Logf("%s STDERR\n %s", cmd.Path, stderr.String())
			}
			if stdout.Len() > 0 {
				t.Logf("%s STDOUT\n %s", cmd.Path, stdout.String())
			}

			return err
		}, func(error) {
			_ = cmd.Process.Signal(syscall.SIGTERM)
		})
	}

	var exit = make(chan error, 1)
	go func(g run.Group) {
		exit <- g.Run()
		close(exit)
	}(g)

	return exit, nil
}
