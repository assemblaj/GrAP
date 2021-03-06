package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"math/rand"
	"os"
	"os/signal"
	"reflect"
	"runtime"
	"sort"
	"sync"
	"syscall"
	"time"

	libp2p "github.com/libp2p/go-libp2p"
	crypto "github.com/libp2p/go-libp2p-crypto"
	discovery "github.com/libp2p/go-libp2p-discovery"
	libp2pdht "github.com/libp2p/go-libp2p-kad-dht"
	peerstore "github.com/libp2p/go-libp2p-peerstore"
	ps "github.com/libp2p/go-libp2p-peerstore"
	ma "github.com/multiformats/go-multiaddr"
	multiaddr "github.com/multiformats/go-multiaddr"
)

// helper method - create a lib-p2p host to listen on a port
func makeRandomNode(port int, done chan bool, gravData *GravitationData) *Node {
	// Ignoring most errors for brevity
	// See echo example for more details and better implementation
	priv, _, _ := crypto.GenerateKeyPair(crypto.Secp256k1, 256)
	listen, _ := ma.NewMultiaddr(fmt.Sprintf("/ip4/127.0.0.1/tcp/%d", port))
	host, _ := libp2p.New(
		context.Background(),
		libp2p.ListenAddrs(listen),
		libp2p.Identity(priv),
		libp2p.DisableRelay(),
	)

	return NewNode(host, done, gravData)
}

type TestData struct {
	TestNetwork map[string][]string
	TestOrbit   []string
	TestingOn   string
}

func testGravitation(fname string) bool {
	// Read test data from file
	testConfig := TestData{}
	b, err := ioutil.ReadFile(fname)
	if err != nil {
		log.Println("Error reading data from file. ")
	}
	err = json.Unmarshal(b, &testConfig)

	if err != nil {
		log.Println("Error loading data. ")
	}

	done := make(chan bool, 1)

	// Set up test network
	hostMap := make(map[string]*Node)
	orbitPeerIds := []string{}
	for k, v := range testConfig.TestNetwork {
		rand.Seed(666)
		port := rand.Intn(100) + 10000

		if _, exist := hostMap[k]; !exist {
			profile := []string{time.Now().Format("20060102150405")}

			gravData := GravitationData{Profile: profile}
			hostMap[k] = makeRandomNode(port, done, &gravData)
		}
		for _, peer := range v {

			if _, exist := hostMap[peer]; !exist {
				rand.Seed(666)
				newPort := port + 1

				testGravData := GravitationData{Profile: []string{"test"}, Orbit: []Body{}}
				inOrbit := false

				// if part of orbit
				for _, hostName := range testConfig.TestOrbit {
					if hostName == peer {
						inOrbit = true
						testGravData.Profile = hostMap[k].gravData.Profile
					}
				}

				// Creates host and creates peer relationship between it and the root peer
				hostMap[peer] = makeRandomNode(newPort, done, &testGravData)
				if inOrbit {
					orbitPeerIds = append(orbitPeerIds, hostMap[peer].ID().String())
				}
				hostMap[k].Peerstore().AddAddrs(hostMap[peer].ID(), hostMap[peer].Addrs(), ps.PermanentAddrTTL)
				hostMap[peer].Peerstore().AddAddrs(hostMap[k].ID(), hostMap[k].Addrs(), ps.PermanentAddrTTL)
				log.Printf("This is a conversation between %s and %s\n", hostMap[k].ID(), hostMap[peer].ID())

				// Perform gravitation
				hostMap[k].Gravitation(hostMap[peer].Host)

			}
		}
	}

	// Waits for process to finish
	time.Sleep(2 * time.Second)

	actualOrbitIds := []string{}
	for _, data := range hostMap[testConfig.TestingOn].gravData.Orbit {
		actualOrbitIds = append(actualOrbitIds, data.peerID)
	}

	sort.Strings(actualOrbitIds)
	sort.Strings(orbitPeerIds)

	for i := 0; i < 4; i++ {
		<-done
	}

	return reflect.DeepEqual(actualOrbitIds, orbitPeerIds)
}

func gravitationRendezvous(config Config, gravData *GravitationData) { //profile []string, orbit []Body) {
	done := make(chan bool, 1)

	ctx := context.Background()

	// libp2p.New constructs a new libp2p Host. Other options can be added
	// here.
	priv, _, _ := crypto.GenerateKeyPair(crypto.Secp256k1, 256)

	host, err := libp2p.New(
		ctx,
		libp2p.ListenAddrs([]multiaddr.Multiaddr(config.ListenAddresses)...),
		libp2p.Identity(priv),
		libp2p.DisableRelay(),
	)
	if err != nil {
		panic(err)
	}

	node := NewNode(host, done, gravData)

	// ----------------------------
	// Start a DHT, for use in peer discovery. We can't just make a new DHT
	// client because we want each peer to maintain its own local copy of the
	// DHT, so that the bootstrapping node of the DHT can go down without
	// inhibiting future peer discovery.
	kademliaDHT, err := libp2pdht.New(ctx, host)
	if err != nil {
		panic(err)
	}

	// Bootstrap the DHT. In the default configuration, this spawns a Background
	// thread that will refresh the peer table every five minutes.
	log.Println("Bootstrapping the DHT")
	if err = kademliaDHT.Bootstrap(ctx); err != nil {
		panic(err)
	}

	// Let's connect to the bootstrap nodes first. They will tell us about the
	// other nodes in the network.
	var wg sync.WaitGroup
	for _, peerAddr := range config.BootstrapPeers {
		peerinfo, _ := peerstore.InfoFromP2pAddr(peerAddr)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := host.Connect(ctx, *peerinfo); err != nil {
				log.Println(err)
			} else {
				log.Println("Connection established with bootstrap node:", *peerinfo)
			}
		}()
	}
	wg.Wait()

	// We use a rendezvous point "meet me here" to announce our location.
	// This is like telling your friends to meet you at the Eiffel Tower.
	log.Println("Announcing ourselves...")
	routingDiscovery := discovery.NewRoutingDiscovery(kademliaDHT)
	discovery.Advertise(ctx, routingDiscovery, config.RendezvousString)
	log.Println("Successfully announced!")

	// Now, look for others who have announced
	// This is like your friend telling you the location to meet you.
	log.Println("Searching for other peers...")
	peerChan, err := routingDiscovery.FindPeers(ctx, config.RendezvousString)
	if err != nil {
		panic(err)
	}

	for peer := range peerChan {
		if peer.ID == host.ID() {
			continue
		}
		log.Println("Found peer:", peer)

		log.Println("Connecting to:", peer)
		node.GravitationPeerID(peer.ID)
		log.Println("Connected to:", peer)
	}

	select {}
}

const help = `
Creates Gravitation protocol instance. 
Usage: 
./GravitationProtocol 
  - runs default gravitation test 
./GravitationProtocol -t testfile 
  - runs a gravitation protocol test with given test file 
`

func main() {
	// // Parse some flags
	config, err := ParseFlags()
	if err != nil {
		panic(err)
	}

	var gravData GravitationData

	// Stop things when you ctl-c, and other ways, because.
	// Cleanup stuff
	term := make(chan os.Signal)
	signal.Notify(term, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case <-term:
			// Print out a message
			log.Println("==> Stopping Gravitation Protocol")

			if config.SaveFile != "" {
				log.Printf("Saving data to file: %s", config.SaveFile)
				WriteGravData(config.SaveFile, &gravData)
			}

			// Cleanup GC
			runtime.GC()

			// Handle other specific stuff like closing HTTP sockets
			// that may persist
			os.Exit(1)
		}
	}()

	if config.LoadFile != "" {
		ReadGravData(config.LoadFile, &gravData)
	} else {
		var profile []string
		if config.Profile != nil {
			profile = config.Profile
		} else {
			profile = []string{"test", "test2", "test3"}
		}
		orbit := []Body{}
		gravData = GravitationData{Profile: profile, Orbit: orbit}
	}

	if config.TestFile != "" {
		if testGravitation(config.TestFile) {
			log.Println("Test successful!")
		} else {
			log.Println("Test failed.")
		}
	} else {
		gravitationRendezvous(config, &gravData)
	}

}
