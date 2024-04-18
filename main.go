package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"log/slog"

	"gopkg.in/yaml.v3"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"

	"nefelim4ag/aws-ondemand-proxy/pkg/tcpserver"
)

type config struct {
	Resource    string        `yaml:"resource"`
	IdleTimeout time.Duration `yaml:"idle-timeout"`
	Proxy       []proxyPair   `yaml:"proxy"`
}

type proxyPair struct {
	Local  string `yaml:"local"`
	Remote string `yaml:"remote"`
}

type globalState struct {
	lastServe atomic.Int64
	stateCode atomic.Int32
	inOutPort map[int]string

	id       string
	instance *ec2.Instance

	ec2client *ec2.EC2

	servers []tcpserver.Server
}

func (state *globalState) touch() {
	state.lastServe.Store(time.Now().Unix())
}

func (state *globalState) pipe(src *net.TCPConn, dst *net.TCPConn) {
	defer src.Close()
	defer dst.Close()
	buf := make([]byte, 1024)

	for {
		state.touch()
		n, err := dst.ReadFrom(src)
		if err != nil {
			return
		}
		// Use blocking IO
		if n == 0 {
			n, err := src.Read(buf)
			if err != nil {
				return
			}
			b := buf[:n]

			state.touch()
			n, err = dst.Write(b)
			if err != nil {
				return
			}
		}
	}
}

func addrToPort(address string) (int, error) {
	srvSplit := strings.Split(address, ":")
	portString := srvSplit[len(srvSplit)-1]
	localPort, err := strconv.ParseInt(portString, 10, 32)
	if err != nil {
		return 0, err
	}
	return int(localPort), nil
}

func (state *globalState) connectionHandler(clientConn *net.TCPConn, err error) {
	if err != nil {
		slog.Error(err.Error())
		return
	}
	defer clientConn.Close()

	localPort, err := addrToPort(clientConn.LocalAddr().String())
	if err != nil {
		slog.Error(err.Error())
		return
	}
	upstreamRawAddr := state.inOutPort[int(localPort)]

	state.touch()
	// Must be some sort of locking, or sync.Cond, but I'm too lazy.
	for state.stateCode.Load() != 16 { // Running
		state.touch()
		time.Sleep(time.Second * 3)
	}

	var serverConn *net.TCPConn
	// Retry up to ~60s
	deadline := time.Now().Add(time.Second * 60)
	for t := time.Now(); t.Before(deadline); t = time.Now() {
		state.touch()
		// DNS can & will change sometimes, so resolve it each time
		conn, err := net.DialTimeout("tcp", upstreamRawAddr, time.Second*time.Duration(10))
		if err == nil {
			serverConn = conn.(*net.TCPConn)
			break
		}

		netErr := err.(*net.OpError)
		switch netErr.Err.Error() {
		case "connect: connection refused":
			slog.Error(err.Error(), "temporary", "maybe", "retry", true, "abortInSec", deadline.Unix()-time.Now().Unix())
			time.Sleep(time.Second * time.Duration(5))
		case "i/o timeout":
			slog.Error(err.Error(), "temporary", "maybe", "retry", true, "abortInSec", deadline.Unix()-time.Now().Unix())
		default:
			slog.Error(err.Error())
			return
		}
	}

	// If retry doesn't help - abort client
	if err != nil || serverConn == nil {
		return
	}

	serverConn.SetKeepAlive(true)
	slog.Info("Handle connection", "client", clientConn.RemoteAddr().String(), "server", serverConn.RemoteAddr().String())

	// Handle connection close internal in pipe, close both ends in same time
	go state.pipe(clientConn, serverConn)
	state.pipe(serverConn, clientConn)
}

func (state *globalState) Scaler(timeout time.Duration, replicas int32) {
	for range time.NewTicker(time.Second * 5).C {
		now := time.Now().Unix()
		timeoutSec := int64(timeout.Seconds())
		lastAccess := state.lastServe.Load()
		if (lastAccess + timeoutSec) < now {
			state.updateScale(0)
		} else {
			state.updateScale(1)
		}
	}
}

func (state *globalState) updateScale(replicas int32) {
	state.describeInstance()
	toStop := &ec2.StopInstancesInput{
		InstanceIds: []*string{
			aws.String(state.id),
		},
	}
	toStart := &ec2.StartInstancesInput{
		InstanceIds: []*string{
			aws.String(state.id),
		},
	}

	switch replicas {
	case 0:
		if state.stateCode.Load() == 80 { // Stopped
			return
		}
		if state.stateCode.Load() == 64 { // Stopping
			return
		}
		result, err := state.ec2client.StopInstances(toStop)
		if err != nil {
			slog.Error(err.Error())
		}
		slog.Info("Stopping", slog.String("previous", *result.StoppingInstances[0].PreviousState.Name), slog.String("current", *result.StoppingInstances[0].CurrentState.Name))
	case 1:
		if state.stateCode.Load() == 16 { // Running
			return
		}
		result, err := state.ec2client.StartInstances(toStart)
		if err != nil {
			slog.Error(err.Error())
		}
		slog.Info("Stopping", slog.String("previous", *result.StartingInstances[0].PreviousState.Name), slog.String("current", *result.StartingInstances[0].CurrentState.Name))
	}
}

func (state *globalState) describeInstance() {
	params := &ec2.DescribeInstancesInput{
		Filters: []*ec2.Filter{
			{
				Name:   aws.String("instance-id"),
				Values: []*string{aws.String(state.id)},
			},
		},
	}

	result, err := state.ec2client.DescribeInstances(params)
	if err != nil {
		slog.Error(err.Error())
		os.Exit(1)
	}

	if len(result.Reservations) == 0 {
		slog.Error("Can't find instance", slog.String("ID", state.id))
		os.Exit(1)
	}
	r := result.Reservations[0]

	if len(r.Instances) == 0 {
		slog.Error("Can't find instance", slog.String("ID", state.id))
		os.Exit(1)
	}
	i := r.Instances[0]

	if len(i.NetworkInterfaces) == 0 {
		slog.Error("Empty network interfaces on instance", slog.String("ID", state.id))
		os.Exit(1)
	}

	if *i.State.Code == 48 {
		slog.Error("Instance terminated", slog.String("ID", state.id))
		os.Exit(1)
	}

	state.instance = i
	state.stateCode.Store(int32(*i.State.Code))
}

func main() {
	var configPath string

	flag.StringVar(&configPath, "config", "", "Yaml config path")
	flag.Parse()

	programLevel := new(slog.LevelVar)
	replace := func(groups []string, a slog.Attr) slog.Attr {
		if a.Key == "msg" {
			a.Key = "message"
		}
		return a
	}
	logger := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: programLevel, ReplaceAttr: replace})
	slog.SetDefault(slog.New(logger))

	if configPath == "" {
		configPath = os.Getenv("CONFIGPATH")
	}

	f, err := os.ReadFile(configPath)
	if err != nil {
		slog.Error(err.Error())
		os.Exit(1)
	}

	c := config{}
	if err := yaml.Unmarshal(f, &c); err != nil {
		slog.Error(err.Error())
		os.Exit(1)
	}

	if len(c.Proxy) == 0 {
		slog.Error("Can't empty proxy section in config")
		os.Exit(1)
	}

	// Load session from shared config
	sess := session.Must(session.NewSessionWithOptions(session.Options{
		SharedConfigState: session.SharedConfigEnable,
	}))

	state := globalState{
		ec2client: ec2.New(sess),
		id:        c.Resource,
	}

	state.describeInstance()

	fmt.Println(state.instance)
	slog.Info("Instance has addresses", slog.String("IPv4", *state.instance.PrivateIpAddress))
	if len(state.instance.NetworkInterfaces[0].Ipv6Addresses) != 0 {
		slog.Info("Instance has addresses", slog.String("IPv6", *state.instance.Ipv6Address))
	}
	slog.Info("Instance has state", slog.String("Name", *state.instance.State.Name), slog.Int("Code", int(*state.instance.State.Code)))

	state.inOutPort = make(map[int]string, len(c.Proxy))

	state.touch()
	go state.Scaler(c.IdleTimeout, 1)

	state.servers = make([]tcpserver.Server, len(c.Proxy))

	for k, p := range c.Proxy {
		addr, err := net.ResolveTCPAddr("tcp", p.Local)
		if err != nil {
			slog.Error("failed to resolve address", "address", p.Local, "error", err.Error())
			os.Exit(1)
		}

		localPort := addr.Port
		var remote string

		if strings.Contains(p.Remote, ":") {
			slog.Info("'remote' contains ':' assume as address", slog.String("remote", p.Remote))
			remote = p.Remote
		} else {
			remote = fmt.Sprintf("%s:%s", *state.instance.PrivateIpAddress, p.Remote)
		}

		_, err = net.ResolveTCPAddr("tcp", remote)
		if err != nil {
			slog.Error("failed to resolve address", remote, err.Error())
			os.Exit(1)
		}
		state.inOutPort[int(localPort)] = remote

		err = state.servers[k].ListenAndServe(addr, state.connectionHandler)
		if err != nil {
			slog.Error(err.Error())
			os.Exit(1)
		}
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	<-sigChan
	slog.Info("Shutting down server...")
	for k := range state.servers {
		state.servers[k].Stop()
	}
	slog.Info("Server stopped.")
}
