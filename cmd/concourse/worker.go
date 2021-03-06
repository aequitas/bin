package main

import (
	"fmt"
	"os"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/concourse/atc"
	"github.com/pivotal-golang/lager"
	"github.com/tedsuo/ifrit"
	"github.com/tedsuo/ifrit/grouper"
	"github.com/tedsuo/ifrit/restart"
	"github.com/tedsuo/ifrit/sigmon"
)

type WorkerCommand struct {
	Name string   `long:"name" description:"The name to set for the worker during registration. If not specified, the hostname will be used."`
	Tags []string `long:"tag" description:"A tag to set during registration. Can be specified multiple times."`

	WorkDir string `long:"work-dir" required:"true" description:"Directory in which to place container data."`

	BindIP   IPFlag `long:"bind-ip"   default:"0.0.0.0" description:"IP address on which to listen for the Garden server."`
	BindPort uint16 `long:"bind-port" default:"7777"    description:"Port on which to listen for the Garden server."`

	PeerIP IPFlag `long:"peer-ip" description:"IP used to reach this worker from the ATC nodes. If omitted, the worker will be forwarded through the SSH connection to the TSA."`

	TSA BeaconConfig `group:"TSA Configuration" namespace:"tsa"`

	Baggageclaim struct {
		BindIP   IPFlag `long:"bind-ip"   default:"0.0.0.0" description:"IP address on which to listen for API traffic."`
		BindPort uint16 `long:"bind-port" default:"7788" description:"Port on which to listen for API traffic."`

		ReapInterval time.Duration `long:"reap-interval" default:"10s" description:"Interval on which to reap expired volumes."`
	} `group:"Baggageclaim Configuration" namespace:"baggageclaim"`

	Metrics struct {
		YellerAPIKey      string `long:"yeller-api-key"     description:"Yeller API key. If specified, all errors logged will be emitted."`
		YellerEnvironment string `long:"yeller-environment" description:"Environment to tag on all Yeller events emitted."`
	} `group:"Metrics & Diagnostics"`
}

func (cmd *WorkerCommand) Execute(args []string) error {
	logger := lager.NewLogger("worker")
	logger.RegisterSink(lager.NewWriterSink(os.Stdout, lager.INFO))

	worker, gardenRunner, err := cmd.gardenRunner(logger.Session("garden"), args)
	if err != nil {
		return err
	}

	baggageclaimRunner, err := cmd.baggageclaimRunner(logger.Session("baggageclaim"))
	if err != nil {
		return err
	}

	members := grouper.Members{
		{
			Name:   "garden",
			Runner: gardenRunner,
		},
		{
			Name:   "baggageclaim",
			Runner: baggageclaimRunner,
		},
	}

	if cmd.TSA.WorkerPrivateKey != "" {
		members = append(members, grouper.Member{
			Name:   "beacon",
			Runner: cmd.beaconRunner(logger.Session("beacon"), worker),
		})
	}

	runner := sigmon.New(grouper.NewParallel(os.Interrupt, members))

	return <-ifrit.Invoke(runner).Wait()
}

func (cmd *WorkerCommand) bindAddr() string {
	return fmt.Sprintf("%s:%d", cmd.BindIP, cmd.BindPort)
}

func (cmd *WorkerCommand) workerName() (string, error) {
	if cmd.Name != "" {
		return cmd.Name, nil
	}

	return os.Hostname()
}

func (cmd *WorkerCommand) beaconRunner(logger lager.Logger, worker atc.Worker) ifrit.Runner {
	beacon := Beacon{
		Logger: logger,
		Config: cmd.TSA,
	}

	var beaconRunner ifrit.RunFunc
	if cmd.PeerIP != "" {
		worker.GardenAddr = fmt.Sprintf("%s:%d", cmd.PeerIP, cmd.BindPort)
		worker.BaggageclaimURL = fmt.Sprintf("http://%s:%d", cmd.PeerIP, cmd.Baggageclaim.BindPort)
		beaconRunner = beacon.Register
	} else {
		worker.GardenAddr = fmt.Sprintf("%s:%d", cmd.BindIP, cmd.BindPort)
		worker.BaggageclaimURL = fmt.Sprintf("http://%s:%d", cmd.Baggageclaim.BindIP, cmd.Baggageclaim.BindPort)
		beaconRunner = beacon.Forward
	}

	beacon.Worker = worker

	return restart.Restarter{
		Runner: beaconRunner,
		Load: func(prevRunner ifrit.Runner, prevErr error) ifrit.Runner {
			if _, ok := prevErr.(*ssh.ExitError); !ok {
				logger.Error("restarting", prevErr)
				time.Sleep(5 * time.Second)
				return beaconRunner
			}

			return nil
		},
	}
}
