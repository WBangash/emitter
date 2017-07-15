/**********************************************************************************
* Copyright (c) 2009-2017 Misakai Ltd.
* This program is free software: you can redistribute it and/or modify it under the
* terms of the GNU Affero General Public License as published by the  Free Software
* Foundation, either version 3 of the License, or(at your option) any later version.
*
* This program is distributed  in the hope that it  will be useful, but WITHOUT ANY
* WARRANTY;  without even  the implied warranty of MERCHANTABILITY or FITNESS FOR A
* PARTICULAR PURPOSE.  See the GNU Affero General Public License  for  more details.
*
* You should have  received a copy  of the  GNU Affero General Public License along
* with this program. If not, see<http://www.gnu.org/licenses/>.
************************************************************************************/

package broker

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/emitter-io/emitter/collection"
	"github.com/emitter-io/emitter/config"
	"github.com/emitter-io/emitter/encoding"
	"github.com/emitter-io/emitter/logging"
	"github.com/emitter-io/emitter/network/address"
	"github.com/emitter-io/emitter/network/listener"
	"github.com/emitter-io/emitter/network/tcp"
	"github.com/emitter-io/emitter/network/websocket"
	"github.com/emitter-io/emitter/perf"
	"github.com/emitter-io/emitter/security"
	"github.com/hashicorp/memberlist"
	"github.com/hashicorp/serf/serf"
)

// Service represents the main structure.
type Service struct {
	Closing          chan bool                 // The channel for closing signal.
	Counters         *perf.Counters            // The performance counters for this service.
	Cipher           *security.Cipher          // The cipher to use for decoding and encoding keys.
	License          *security.License         // The licence for this emitter server.
	Config           *config.Config            // The configuration for the service.
	ContractProvider security.ContractProvider // The contract provider for the service.
	subscriptions    *SubscriptionTrie         // The subscription matching trie.
	http             *http.Server              // The underlying HTTP server.
	tcp              *tcp.Server               // The underlying TCP server.
	cluster          *serf.Serf                // The gossip-based cluster mechanism.
	peers            *collection.ConcurrentMap // The map of all the connected peers for this server.
	events           chan serf.Event           // The channel for receiving gossip events.
	name             string                    // The name of the service.
}

// NewService creates a new service.
func NewService(cfg *config.Config) (s *Service, err error) {
	s = &Service{
		Closing:       make(chan bool),
		Counters:      perf.NewCounters(),
		Config:        cfg,
		subscriptions: NewSubscriptionTrie(),
		events:        make(chan serf.Event),
		http:          new(http.Server),
		tcp:           new(tcp.Server),
	}

	// Attach handlers
	s.tcp.Handler = s.onAcceptConn
	http.HandleFunc("/", s.onRequest)

	// Parse the license
	logging.LogAction("service", "external address is "+address.External().String())
	logging.LogAction("service", "reading the license...")
	if s.License, err = security.ParseLicense(cfg.License); err != nil {
		return nil, err
	}

	// Create a new cipher from the licence provided
	if s.Cipher, err = s.License.Cipher(); err != nil {
		return nil, err
	}

	return s, nil

}

// Creates a configuration for the cluster
func (s *Service) clusterConfig(cfg *config.Config) *serf.Config {
	c := serf.DefaultConfig()
	c.RejoinAfterLeave = true
	c.NodeName = address.Fingerprint() //fmt.Sprintf("%s:%d", address.External().String(), cfg.Cluster.Port) // TODO: fix this
	c.EventCh = s.events
	c.SnapshotPath = cfg.Cluster.SnapshotPath
	c.MemberlistConfig = memberlist.DefaultWANConfig()
	c.MemberlistConfig.BindPort = cfg.Cluster.Gossip
	c.MemberlistConfig.AdvertisePort = cfg.Cluster.Gossip
	c.MemberlistConfig.SecretKey = cfg.Cluster.Key()

	// Set the node name
	c.NodeName = cfg.Cluster.NodeName
	if c.NodeName == "" {
		c.NodeName = fmt.Sprintf("%s%d", address.Fingerprint(), cfg.Cluster.Gossip)
	}
	s.name = c.NodeName

	// Use the public IP address if necessary
	if cfg.Cluster.AdvertiseAddr == "public" {
		c.MemberlistConfig.AdvertiseAddr = address.External().String()
	}

	// Configure routing
	c.Tags["route"] = fmt.Sprintf("%s:%d", c.MemberlistConfig.AdvertiseAddr, cfg.Cluster.Route)
	return c
}

// Name returns the local service name.
func (s *Service) Name() string {
	return s.name
}

// Listens to incoming cluster events.
func (s *Service) clusterEventLoop() {
	for {
		select {
		case <-s.Closing:
			return
		case e := <-s.events:
			if e.EventType() == serf.EventUser {
				event := e.(serf.UserEvent)
				if err := s.onEvent(&event); err != nil {
					logging.LogError("service", "event received", err)
				}
			}
		}
	}
}

// Listen starts the service.
func (s *Service) Listen() (err error) {
	defer s.Close()
	s.hookSignals()

	// Create the cluster if required
	if s.Config.Cluster != nil {
		if s.cluster, err = serf.Create(s.clusterConfig(s.Config)); err != nil {
			return err
		}

		// Listen on cluster event loop
		go s.clusterEventLoop()
		if err := tcp.ServeAsync(s.Config.Cluster.Route, s.Closing, s.onAcceptPeer); err != nil {
			panic(err)
		}
	}

	// Join our seed
	s.Join(s.Config.Cluster.Seed)

	go func() {
		for {
			members := []string{}
			for _, m := range s.cluster.Members() {
				members = append(members, fmt.Sprintf("%s (%s)", m.Name, m.Status.String()))
			}

			println(strings.Join(members, ", "))
			time.Sleep(1000 * time.Millisecond)
		}
	}()

	// Setup the HTTP server
	logging.LogAction("service", "starting the listener...")
	l, err := listener.New(s.Config.TCPPort)
	if err != nil {
		panic(err)
	}

	l.ServeAsync(listener.MatchHTTP(), s.http.Serve)
	l.ServeAsync(listener.MatchAny(), s.tcp.Serve)

	// Serve the listener
	if l.Serve(); err != nil {
		logging.LogError("service", "starting the listener", err)
	}

	return nil
}

// Join attempts to join a set of existing peers.
func (s *Service) Join(peers ...string) error {
	_, err := s.cluster.Join(peers, true)
	return err
}

// Broadcast is used to broadcast a custom user event with a given name and
// payload. The events must be fairly small, and if the  size limit is exceeded
// and error will be returned. If coalesce is enabled, nodes are allowed to
// coalesce this event.
func (s *Service) Broadcast(name string, message interface{}) error {
	buffer, err := encoding.Encode(message)
	if err != nil {
		return err
	}

	return s.cluster.UserEvent(name, buffer, true)
}

// Occurs when a new client connection is accepted.
func (s *Service) onAcceptConn(t net.Conn) {
	conn := s.newConn(t)
	go conn.Process()
}

// Occurs when a new peer connection is accepted.
func (s *Service) onAcceptPeer(t net.Conn) {

}

// Occurs when a new HTTP request is received.
func (s *Service) onRequest(w http.ResponseWriter, r *http.Request) {
	if ws, ok := websocket.TryUpgrade(w, r); ok {
		s.onAcceptConn(ws)
		return
	}
}

// Occurs when a new cluster event is received.
func (s *Service) onEvent(e *serf.UserEvent) error {
	switch e.Name {
	case "+":
		// This is a subscription event which occurs when a client is subscribed to a node.
		var event SubscriptionEvent
		encoding.Decode(e.Payload, &event)

		if event.Node != s.Name() {
			fmt.Printf("%+v\n", event)
		}

	case "-":
		// This is an unsubscription event which occurs when a client is unsubscribed from a node.
		var event SubscriptionEvent
		encoding.Decode(e.Payload, &event)

		if event.Node != s.Name() {
			fmt.Printf("%+v\n", event)
		}

	default:
		return errors.New("received unknown event name: " + e.Name)
	}

	return nil
}

// OnSignal starts the signal processing and makes su
func (s *Service) hookSignals() {
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		for sig := range c {
			s.OnSignal(sig)
		}
	}()
}

// OnSignal will be called when a OS-level signal is received.
func (s *Service) OnSignal(sig os.Signal) {
	switch sig {
	case syscall.SIGTERM:
		fallthrough
	case syscall.SIGINT:
		logging.LogAction("service", fmt.Sprintf("received signal %s, exiting...", sig.String()))
		s.Close()
		os.Exit(0)
	}
}

// Close closes gracefully the service.,
func (s *Service) Close() {
	_ = logging.Flush()

	// Gracefully leave the cluster and shutdown the listener.
	if s.cluster != nil {
		_ = s.cluster.Leave()
		_ = s.cluster.Shutdown()
	}

	// Notify we're closed
	close(s.Closing)
}
