package registrar_test

import (
	"encoding/json"
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/cloudfoundry/gibson"
	"github.com/cloudfoundry/yagnats"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	"github.com/cloudfoundry-incubator/route-registrar/config"
	"github.com/cloudfoundry-incubator/route-registrar/healthchecker/fakes"
	"github.com/cloudfoundry-incubator/route-registrar/registrar"
	"github.com/pivotal-golang/lager"
	"github.com/pivotal-golang/lager/lagertest"
)

var _ = Describe("Registrar.RegisterRoutes", func() {
	var (
		rrConfig      config.Config
		testSpyClient *yagnats.Client

		logger           lager.Logger
		messageBusServer config.MessageBusServer
	)

	BeforeEach(func() {
		natsCmd = startNats(natsPort)

		messageBusServer = config.MessageBusServer{
			fmt.Sprintf("127.0.0.1:%d", natsPort),
			"nats",
			"nats",
		}

		logger = lagertest.NewTestLogger("Registrar test")
		testSpyClient = yagnats.NewClient()

		connectionInfo := yagnats.ConnectionInfo{
			messageBusServer.Host,
			messageBusServer.User,
			messageBusServer.Password,
			nil,
		}

		err := testSpyClient.Connect(&connectionInfo)
		Expect(err).NotTo(HaveOccurred())

		rrConfig = config.Config{
			// doesn't matter if these are the same, just want to send a slice
			MessageBusServers: []config.MessageBusServer{messageBusServer, messageBusServer},
		}
	})

	AfterEach(func() {
		testSpyClient.Disconnect()
		stopCmd(natsCmd)
	})

	Context("When single external host is provided", func() {
		BeforeEach(func() {
			rrConfig.ExternalHost = "riakcs.vcap.me"
			rrConfig.ExternalIp = "127.0.0.1"
			rrConfig.Port = 8080
		})

		It("Sends a router.register message and does not send a router.unregister message", func() {
			// Detect when a router.register message gets sent
			var registered chan (string)
			registered = subscribeToRegisterEvents(testSpyClient, func(msg *yagnats.Message) {
				registered <- string(msg.Payload)
			})

			// Detect when an unregister message gets sent
			var unregistered chan (bool)
			unregistered = subscribeToUnregisterEvents(testSpyClient, func(msg *yagnats.Message) {
				unregistered <- true
			})

			go func() {
				r := registrar.NewRegistrar(rrConfig, logger)
				r.RegisterRoutes()
			}()

			// Assert that we got the right router.register message
			var receivedMessage string
			Eventually(registered, 2).Should(Receive(&receivedMessage))

			expectedRegistryMessage := gibson.RegistryMessage{
				URIs: []string{"riakcs.vcap.me"},
				Host: "127.0.0.1",
				Port: 8080,
			}

			var registryMessage gibson.RegistryMessage
			err := json.Unmarshal([]byte(receivedMessage), &registryMessage)
			Expect(err).ShouldNot(HaveOccurred())

			Expect(registryMessage.URIs).To(Equal(expectedRegistryMessage.URIs))
			Expect(registryMessage.Host).To(Equal(expectedRegistryMessage.Host))
			Expect(registryMessage.Port).To(Equal(expectedRegistryMessage.Port))

			// Assert that we never got a router.unregister message
			Consistently(unregistered, 2).ShouldNot(Receive())
		})

		It("Emits a router.unregister message when SIGINT is sent to the registrar's signal channel", func() {
			verifySignalTriggersUnregister(
				rrConfig,
				syscall.SIGINT,
				logger,
				testSpyClient,
			)
		})

		It("Emits a router.unregister message when SIGTERM is sent to the registrar's signal channel", func() {
			verifySignalTriggersUnregister(
				rrConfig,
				syscall.SIGTERM,
				logger,
				testSpyClient,
			)
		})

		Context("When the registrar has a healthchecker", func() {
			BeforeEach(func() {
				healthCheckerConfig := config.HealthCheckerConf{
					Name:     "a_useful_health_checker",
					Interval: 1,
				}

				rrConfig.HealthChecker = &healthCheckerConfig
			})

			It("Emits a router.unregister message when registrar's health check fails, and emits a router.register message when registrar's health check back to normal", func() {
				healthy := fakes.NewFakeHealthChecker()
				healthy.CheckReturns(true)

				unregistered := make(chan string)
				registered := make(chan string)
				var r *registrar.Registrar

				// Listen for a router.unregister event, then set health status to true, then listen for a router.register event
				subscribeToRegisterEvents(testSpyClient, func(msg *yagnats.Message) {
					registered <- string(msg.Payload)

					healthy.CheckReturns(false)

					subscribeToUnregisterEvents(testSpyClient, func(msg *yagnats.Message) {
						unregistered <- string(msg.Payload)
					})
				})

				go func() {
					r = registrar.NewRegistrar(rrConfig, logger)
					r.AddHealthCheckHandler(healthy)
					r.RegisterRoutes()
				}()

				var receivedMessage string
				testTimeout := rrConfig.HealthChecker.Interval * 3

				expectedRegistryMessage := gibson.RegistryMessage{
					URIs: []string{"riakcs.vcap.me"},
					Host: "127.0.0.1",
					Port: 8080,
				}

				var registryMessage gibson.RegistryMessage

				Eventually(registered, testTimeout).Should(Receive(&receivedMessage))
				err := json.Unmarshal([]byte(receivedMessage), &registryMessage)
				Expect(err).ShouldNot(HaveOccurred())

				Expect(registryMessage.URIs).To(Equal(expectedRegistryMessage.URIs))
				Expect(registryMessage.Host).To(Equal(expectedRegistryMessage.Host))
				Expect(registryMessage.Port).To(Equal(expectedRegistryMessage.Port))

				Eventually(unregistered, testTimeout).Should(Receive(&receivedMessage))
				err = json.Unmarshal([]byte(receivedMessage), &registryMessage)
				Expect(err).ShouldNot(HaveOccurred())

				Expect(registryMessage.URIs).To(Equal(expectedRegistryMessage.URIs))
				Expect(registryMessage.Host).To(Equal(expectedRegistryMessage.Host))
				Expect(registryMessage.Port).To(Equal(expectedRegistryMessage.Port))
			})
		})

	})

	Context("When backing legacy route registration", func() {
		BeforeEach(func() {
			rrConfig.RefreshInterval = 1
		})

		Context("one route, multiple URIs", func() {
			BeforeEach(func() {
				rrConfig.Host = "my host"
				rrConfig.RefreshInterval = 500 * time.Millisecond
				rrConfig.Routes = []config.Route{
					{
						Name: "my route",
						Port: 8080,
						URIs: []string{
							"my uri 1",
							"my uri 2",
						},
					},
				}
			})

			It("periodically registers all URIs for all URIs associated with the route", func() {
				// Detect when a router.register message gets sent
				var registered chan (string)
				registered = subscribeToRegisterEvents(testSpyClient, func(msg *yagnats.Message) {
					registered <- string(msg.Payload)
				})

				// Detect when an unregister message gets sent
				var unregistered chan (bool)
				unregistered = subscribeToUnregisterEvents(testSpyClient, func(msg *yagnats.Message) {
					unregistered <- true
				})

				r := registrar.NewRegistrar(rrConfig, logger)
				go func() {
					r.RegisterRoutes()
				}()

				// Assert that we got the right router.register message
				var receivedMessage string
				Eventually(registered, 2).Should(Receive(&receivedMessage))

				var registryMessage gibson.RegistryMessage
				err := json.Unmarshal([]byte(receivedMessage), &registryMessage)
				Expect(err).ShouldNot(HaveOccurred())

				expectedRegistryMessage := gibson.RegistryMessage{
					URIs: rrConfig.Routes[0].URIs,
					Host: rrConfig.Host,
					Port: rrConfig.Routes[0].Port,
				}

				Expect(registryMessage.URIs).To(Equal(expectedRegistryMessage.URIs))
				Expect(registryMessage.Host).To(Equal(expectedRegistryMessage.Host))
				Expect(registryMessage.Port).To(Equal(expectedRegistryMessage.Port))

				// Assert that we never got a router.unregister message
				Consistently(unregistered, 2).ShouldNot(Receive())
			})
		})
	})
})

func verifySignalTriggersUnregister(
	rrConfig config.Config,
	signal os.Signal,
	logger lager.Logger,
	testSpyClient *yagnats.Client,
) {
	unregistered := make(chan string)
	returned := make(chan bool)

	var r *registrar.Registrar

	// Trigger a SIGINT after a successful router.register message
	subscribeToRegisterEvents(testSpyClient, func(msg *yagnats.Message) {
		r.SignalChannel <- signal
	})

	// Detect when a router.unregister message gets sent
	subscribeToUnregisterEvents(testSpyClient, func(msg *yagnats.Message) {
		unregistered <- string(msg.Payload)
	})

	go func() {
		r = registrar.NewRegistrar(rrConfig, logger)
		r.RegisterRoutes()

		// Set up a channel to wait for RegisterRoutes to return
		returned <- true
	}()

	// Assert that we got the right router.unregister message as a result of the signal
	var receivedMessage string
	Eventually(unregistered, 2).Should(Receive(&receivedMessage))

	expectedRegistryMessage := gibson.RegistryMessage{
		URIs: []string{"riakcs.vcap.me"},
		Host: "127.0.0.1",
		Port: 8080,
	}

	var registryMessage gibson.RegistryMessage
	err := json.Unmarshal([]byte(receivedMessage), &registryMessage)

	Expect(err).ShouldNot(HaveOccurred())
	Expect(registryMessage.URIs).To(Equal(expectedRegistryMessage.URIs))
	Expect(registryMessage.Host).To(Equal(expectedRegistryMessage.Host))
	Expect(registryMessage.Port).To(Equal(expectedRegistryMessage.Port))

	// Assert that RegisterRoutes returned
	Expect(returned).To(Receive())
}

func subscribeToRegisterEvents(testSpyClient *yagnats.Client, callback func(msg *yagnats.Message)) (registerChannel chan string) {
	registerChannel = make(chan string)
	go testSpyClient.Subscribe("router.register", callback)

	return
}

func subscribeToUnregisterEvents(testSpyClient *yagnats.Client, callback func(msg *yagnats.Message)) (unregisterChannel chan bool) {
	unregisterChannel = make(chan bool)
	go testSpyClient.Subscribe("router.unregister", callback)

	return
}
