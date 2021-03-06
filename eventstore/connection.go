package goes

import (
	"fmt"
	"log"
	"net"
	"time"

	"sync"

	"github.com/golang/protobuf/proto"
	"github.com/pgermishuys/goes/protobuf"
	"github.com/satori/go.uuid"
)

// Configuration for an Event Store Connection
type Configuration struct {
	Address             string
	Port                int
	Login               string
	Password            string
	ReconnectionDelay   int
	MaxReconnects       int
	MaxOperationRetries int
	EndpointDiscoverer  EndpointDiscoverer
}

// EventStoreConnection will manage the lifetime and connection to an Event Store Node/Cluster
type EventStoreConnection struct {
	Config        *Configuration
	Socket        *net.TCPConn
	connected     bool
	requests      map[uuid.UUID]chan<- TCPPackage
	subscriptions map[uuid.UUID]*Subscription
	ConnectionID  uuid.UUID
	Mutex         *sync.Mutex
}

// NewConfiguration creates a configuration with default settings
func NewConfiguration() *Configuration {
	return &Configuration{
		ReconnectionDelay:   10000,
		MaxReconnects:       10,
		MaxOperationRetries: 10,
	}
}

// Connect attempts to connect to Event Store using the given configuration
func (connection *EventStoreConnection) Connect() error {
	connection.requests = make(map[uuid.UUID]chan<- TCPPackage)
	connection.subscriptions = make(map[uuid.UUID]*Subscription)
	return connectWithRetries(connection, connection.Config.MaxReconnects)
}

// Close attempts to close the connection to Event Store
func (connection *EventStoreConnection) Close() error {
	connection.Mutex.Lock()
	connection.connected = false
	connection.Mutex.Unlock()
	log.Printf("[info] closing the connection (id: %+v) to event store...\n'", connection.ConnectionID)
	err := connection.Socket.Close()
	connection.Socket = nil
	if err != nil {
		log.Printf("[error] failed closing the connection to event store...%+v\n'", err)
	}
	closeConnection(connection)
	return err
}

// NewEventStoreConnection sets up a new Event Store Connection but does not open the connection
func NewEventStoreConnection(config *Configuration) (*EventStoreConnection, error) {
	if config.EndpointDiscoverer == nil {
		if len(config.Address) == 0 {
			return nil, fmt.Errorf("The address (%v) cannot be an empty string", config.Address)
		}
		if config.Port <= 0 {
			return nil, fmt.Errorf("The port (%v) cannot be less or equal to 0", config.Port)
		}
	}
	conn := &EventStoreConnection{
		Config:       config,
		ConnectionID: uuid.NewV4(),
		Mutex:        &sync.Mutex{},
	}
	log.Printf("[info] created new event store connection : %+v", conn)
	return conn, nil
}

func connectWithRetries(connection *EventStoreConnection, retryAttempts int) error {
	if connection.Config.EndpointDiscoverer != nil {
		memberInfo, err := connection.Config.EndpointDiscoverer.Discover()
		if err != nil {
			return err
		}
		connection.Config.Address = memberInfo.ExternalTCPIP
		connection.Config.Port = memberInfo.ExternalTCPPort
	}
	if retryAttempts > 0 {
		err := connect(connection)
		if err != nil {
			log.Printf("[info] reconnect attempt %v of %v failed: %v", (connection.Config.MaxReconnects-retryAttempts)+1, connection.Config.MaxReconnects, err.Error())
			time.Sleep(time.Duration(connection.Config.ReconnectionDelay) * time.Millisecond)
			//extract to appropriate method
			if connection.Config.EndpointDiscoverer != nil {
				log.Printf("[info] checking nodes")
				memberInfo, _ := connection.Config.EndpointDiscoverer.Discover()
				connection.Config.Address = memberInfo.ExternalTCPIP
				connection.Config.Port = memberInfo.ExternalTCPPort
			}
			return connectWithRetries(connection, retryAttempts-1)
		}
		return nil
	}
	closeConnection(connection)
	return fmt.Errorf("failed to reconnect. Retry limit of %v reached", connection.Config.MaxReconnects)
}

func connect(connection *EventStoreConnection) error {
	log.Printf("[info] connecting (id: %+v) to event store...\n", connection.ConnectionID)

	address := fmt.Sprintf("%s:%v", connection.Config.Address, connection.Config.Port)
	resolvedAddress, err := net.ResolveTCPAddr("tcp", address)
	if err != nil {
		return fmt.Errorf("failed to resolve tcp address %s\n", address)
	}
	conn, err := net.DialTCP("tcp", nil, resolvedAddress)
	if err != nil {
		return fmt.Errorf("failed to connect to event store on %+v. details: %s\n", address, err.Error())
	}
	log.Printf("[info] successfully connected to event store on %s (id: %+v)\n", address, connection.ConnectionID)
	connection.Socket = conn
	connection.connected = true

	go readFromSocket(connection)
	return nil
}

func closeConnection(connection *EventStoreConnection) {
	log.Printf("[error] connection (id: %+v) closed\n", connection.ConnectionID)

	reason := protobuf.SubscriptionDropped_Unsubscribed
	subDropped := &protobuf.SubscriptionDropped{
		Reason: &reason,
	}
	data, err := proto.Marshal(subDropped)
	if err != nil {
		log.Fatal("[fatal] marshalling error: ", err)
	}

	for _, sub := range connection.subscriptions {
		pkg, err := newPackage(subscriptionDropped, data, sub.CorrelationID.Bytes(), connection.Config.Login, connection.Config.Password)
		if err != nil {
			log.Printf("[error] failed to drop subscription %v", sub.CorrelationID)
		}
		sub.Channel <- pkg
	}
	connection.requests = make(map[uuid.UUID]chan<- TCPPackage)
	connection.subscriptions = make(map[uuid.UUID]*Subscription)
}

func readFromSocket(connection *EventStoreConnection) {
	buffer := make([]byte, 40000)
	for {
		connection.Mutex.Lock()
		if connection.connected == false {
			break
		}
		connection.Mutex.Unlock()
		_, err := connection.Socket.Read(buffer)
		if err != nil {
			if connection.connected && err.Error() != "EOF" {
				log.Fatalf("[fatal] (id: %+v) failed to read with %+v\n", connection.ConnectionID, err.Error())
			}
			if err.Error() == "EOF" {
				connection.Close()
				err = connectWithRetries(connection, connection.Config.MaxReconnects)
				if err != nil {
					log.Printf("[error] (id: %+v) %s\n", connection.ConnectionID, err.Error())
				} else {
					log.Printf("[info] connection (id: %+v) reconnected\n", connection.ConnectionID)
				}
			}
			break
		}

		msg, err := parsePackage(buffer)
		if err != nil {
			log.Fatalf("[fatal] could not decode tcp package: %+v\n", err.Error())
		}
		switch msg.Command {
		case heartbeatRequest:
			pkg, err := newPackage(heartbeatResponse, nil, msg.CorrelationID, "", "")
			if err != nil {
				log.Printf("[error] failed to create new heartbeat response package\n")
			}
			channel := make(chan<- TCPPackage)
			go sendPackage(pkg, connection, channel)
			break
		case pong:
			pkg, err := newPackage(ping, nil, uuid.NewV4().Bytes(), "", "")
			if err != nil {
				log.Printf("[error] failed to create new ping response package")
			}
			channel := make(chan<- TCPPackage)
			go sendPackage(pkg, connection, channel)
			break
		case writeEventsCompleted, readEventCompleted, deleteStreamCompleted, readStreamEventsForwardCompleted, readStreamEventsBackwardCompleted, subscriptionConfirmation, streamEventAppeared, createPersistentSubscriptionCompleted, persistentSubscriptionConfirmation:
			correlationID, _ := uuid.FromBytes(msg.CorrelationID)
			if request, ok := connection.requests[correlationID]; ok {
				request <- msg
			}
			break
		case notAuthenticated:
			correlationID, _ := uuid.FromBytes(msg.CorrelationID)
			if request, ok := connection.requests[correlationID]; ok {
				request <- msg
			}
		case 0x0F:
			log.Fatal("[fatal] bad request sent")
			break
		}
	}
}

func sendPackage(pkg TCPPackage, connection *EventStoreConnection, channel chan<- TCPPackage) error {
	correlationID, _ := uuid.FromBytes(pkg.CorrelationID)
	connection.requests[correlationID] = channel
	err := pkg.write(connection)
	if err != nil {
		return err
	}
	return nil
}
