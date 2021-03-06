package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	tunnel "git.sequentialread.com/forest/tunnel/tunnel-lib"
)

type ServerConfig struct {
	DebugLog   bool
	ListenPort int

	// Domain is only used for validating the TLS client certificates
	// when TLS is used.  the cert's Subject CommonName is expected to be <ClientId>@<Domain>
	// I did this because I believe this is a standard for TLS client certs,
	// based on domain users/email addresses.
	Domain string

	UseTls                   bool
	CaCertificateFilesGlob   string
	ServerTlsKeyFile         string
	ServerTlsCertificateFile string
}

type ClientConfig struct {
	DebugLog                 bool
	ClientIdentifier         string
	ServerAddr               string
	UseTls                   bool
	ServiceToLocalAddrMap    *map[string]string
	CaCertificateFilesGlob   string
	ClientTlsKeyFile         string
	ClientTlsCertificateFile string
	AdminUnixSocket          string
}

type ListenerConfig struct {
	HaProxyProxyProtocol bool
	ListenAddress        string
	ListenHostnameGlob   string
	ListenPort           int
	BackEndService       string
	ClientIdentifier     string
}

type ClientState struct {
	CurrentState string
	LastState    string
}

type ManagementHttpHandler struct {
	ControlHandler http.Handler
}

type LiveConfigUpdate struct {
	Listeners             []ListenerConfig
	ServiceToLocalAddrMap map[string]string
}

type adminAPI struct{}

// Server State
var listeners []ListenerConfig
var clientStatesMutex = &sync.Mutex{}
var clientStates map[string]ClientState
var server *tunnel.Server

// Client State
var client *tunnel.Client
var tlsClientConfig *tls.Config
var serverURL *string
var serviceToLocalAddrMap *map[string]string

func main() {

	mode := flag.String("mode", "", "Run client or server application. Allowed values: [client,server]")

	configFileName := flag.String("configFile", "config.json", "File path to JSON configuration file. Default value: config.json")

	flag.Parse()

	if mode != nil && *mode == "server" {
		runServer(configFileName)
	} else if mode != nil && *mode == "client" {
		runClient(configFileName)
	} else {
		fmt.Print("main(): required command line flag '-mode' was not set to one of the allowed values 'client' or 'server'.  Exiting.\n")
		os.Exit(1)
	}

}

// admin api handler for /liveconfig over unix socket
func (handler adminAPI) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	switch path.Clean(request.URL.Path) {
	case "/liveconfig":
		if request.Method == "PUT" {
			requestBytes, err := ioutil.ReadAll(request.Body)
			if err != nil {
				log.Printf("adminAPI: request read error: %+v\n\n", err)
				http.Error(response, "500 request read error", http.StatusInternalServerError)
				return
			}
			var configUpdate LiveConfigUpdate
			err = json.Unmarshal(requestBytes, &configUpdate)
			if err != nil {
				log.Printf("adminAPI: can't parse JSON: %+v\n\n", err)
				http.Error(response, "400 bad request: can't parse JSON", http.StatusBadRequest)
				return
			}

			sendBytes, err := json.Marshal(configUpdate.Listeners)
			if err != nil {
				log.Printf("adminAPI: Listeners json serialization failed: %+v\n\n", err)
				http.Error(response, "500 Listeners json serialization failed", http.StatusInternalServerError)
				return
			}
			apiURL := fmt.Sprintf("https://%s/tunnels", *serverURL)
			tunnelsRequest, err := http.NewRequest("PUT", apiURL, bytes.NewReader(sendBytes))
			if err != nil {
				log.Printf("adminAPI: error creating tunnels request: %+v\n\n", err)
				http.Error(response, "500 error creating tunnels request", http.StatusInternalServerError)
				return
			}
			tunnelsRequest.Header.Add("content-type", "application/json")

			client := &http.Client{
				Transport: &http.Transport{
					TLSClientConfig: tlsClientConfig,
				},
				Timeout: 10 * time.Second,
			}
			tunnelsResponse, err := client.Do(tunnelsRequest)
			if err != nil {
				log.Printf("adminAPI: Do(tunnelsRequest): %+v\n\n", err)
				http.Error(response, "502 tunnels request failed", http.StatusBadGateway)
				return
			}
			tunnelsResponseBytes, err := ioutil.ReadAll(tunnelsResponse.Body)
			if err != nil {
				log.Printf("adminAPI: tunnelsResponse read error: %+v\n\n", err)
				http.Error(response, "502 tunnelsResponse read error", http.StatusBadGateway)
				return
			}

			if tunnelsResponse.StatusCode != http.StatusOK {
				log.Printf(
					"adminAPI: tunnelsRequest returned HTTP %d: %s\n\n",
					tunnelsResponse.StatusCode, string(tunnelsResponseBytes),
				)
				http.Error(
					response,
					fmt.Sprintf("502 tunnels request returned HTTP %d", tunnelsResponse.StatusCode),
					http.StatusBadGateway,
				)
				return
			}

			serviceToLocalAddrMap = &configUpdate.ServiceToLocalAddrMap

			response.Header().Add("content-type", "application/json")
			response.WriteHeader(http.StatusOK)
			response.Write(tunnelsResponseBytes)

		} else {
			response.Header().Set("Allow", "PUT")
			http.Error(response, "405 method not allowed, try PUT", http.StatusMethodNotAllowed)
		}
	default:
		http.Error(response, "404 not found, try PUT /liveconfig", http.StatusNotFound)
	}

}

func runClient(configFileName *string) {

	configBytes := getConfigBytes(configFileName)

	var config ClientConfig
	err := json.Unmarshal(configBytes, &config)
	if err != nil {
		log.Fatalf("runClient(): can't json.Unmarshal(configBytes, &config) because %s \n", err)
	}
	serviceToLocalAddrMap = config.ServiceToLocalAddrMap
	serverURL = &config.ServerAddr

	configToLog, _ := json.MarshalIndent(config, "", "  ")
	log.Printf("theshold client is starting up using config:\n%s\n", string(configToLog))

	dialFunction := net.Dial

	if config.UseTls {

		cert, err := tls.LoadX509KeyPair(config.ClientTlsCertificateFile, config.ClientTlsKeyFile)
		if err != nil {
			log.Fatal(err)
		}

		certificates, err := filepath.Glob(config.CaCertificateFilesGlob)
		if err != nil {
			log.Fatal(err)
		}

		caCertPool := x509.NewCertPool()
		for _, filename := range certificates {
			caCert, err := ioutil.ReadFile(filename)
			if err != nil {
				log.Fatal(err)
			}
			caCertPool.AppendCertsFromPEM(caCert)
		}

		tlsClientConfig = &tls.Config{
			Certificates: []tls.Certificate{cert},
			RootCAs:      caCertPool,
		}
		tlsClientConfig.BuildNameToCertificate()

		dialFunction = func(network, address string) (net.Conn, error) {
			return tls.Dial(network, address, tlsClientConfig)
		}
	}

	clientStateChanges := make(chan *tunnel.ClientStateChange)
	tunnelClientConfig := &tunnel.ClientConfig{
		DebugLog:   config.DebugLog,
		Identifier: config.ClientIdentifier,
		ServerAddr: config.ServerAddr,
		FetchLocalAddr: func(service string) (string, error) {
			//log.Printf("(*serviceToLocalAddrMap): %+v\n\n", (*serviceToLocalAddrMap))
			localAddr, hasLocalAddr := (*serviceToLocalAddrMap)[service]
			if !hasLocalAddr {
				return "", errors.New("service not configured. See ServiceToLocalAddrMap in client config file.")
			}
			return localAddr, nil
		},
		Dial:         dialFunction,
		StateChanges: clientStateChanges,
	}

	client, err = tunnel.NewClient(tunnelClientConfig)
	if err != nil {
		log.Fatalf("runClient(): can't create tunnel client because %s \n", err)
	}

	go (func() {
		for {
			stateChange := <-clientStateChanges
			fmt.Printf("clientStateChange: %s\n", stateChange.String())
		}
	})()

	go runClientAdminApi(config)

	fmt.Print("runClient(): the client should be running now\n")
	client.Start()

}

func runClientAdminApi(config ClientConfig) {

	os.Remove(config.AdminUnixSocket)

	listenAddress, err := net.ResolveUnixAddr("unix", config.AdminUnixSocket)
	if err != nil {
		panic(fmt.Sprintf("runClient(): can't start because net.ResolveUnixAddr() returned %+v", err))
	}

	listener, err := net.ListenUnix("unix", listenAddress)
	if err != nil {
		panic(fmt.Sprintf("can't start because net.ListenUnix() returned %+v", err))
	}
	log.Printf("AdminUnixSocket Listening: %v\n\n", config.AdminUnixSocket)
	defer listener.Close()

	server := http.Server{
		Handler:      adminAPI{},
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	err = server.Serve(listener)
	if err != nil {
		panic(fmt.Sprintf("AdminUnixSocket server returned %+v", err))
	}
}

func runServer(configFileName *string) {

	configBytes := getConfigBytes(configFileName)

	var config ServerConfig
	err := json.Unmarshal(configBytes, &config)
	if err != nil {
		fmt.Printf("runServer(): can't json.Unmarshal(configBytes, &config) because %s \n", err)
		os.Exit(1)
	}

	configToLog, _ := json.MarshalIndent(config, "", "  ")
	log.Printf("threshold server is starting up using config:\n%s\n", string(configToLog))

	clientStateChangeChannel := make(chan *tunnel.ClientStateChange)

	tunnelServerConfig := &tunnel.ServerConfig{
		StateChanges: clientStateChangeChannel,
		Domain:       config.Domain,
		DebugLog:     config.DebugLog,
	}
	server, err = tunnel.NewServer(tunnelServerConfig)
	if err != nil {
		fmt.Printf("runServer(): can't create tunnel server because %s \n", err)
		os.Exit(1)
	}

	clientStates = make(map[string]ClientState)
	go (func() {
		for {
			clientStateChange := <-clientStateChangeChannel
			clientStatesMutex.Lock()
			previousState := ""
			currentState := clientStateChange.Current.String()
			fromMap, wasInMap := clientStates[clientStateChange.Identifier]
			if wasInMap {
				previousState = fromMap.CurrentState
			} else {
				previousState = clientStateChange.Previous.String()
			}
			if clientStateChange.Error != nil && clientStateChange.Error != io.EOF {
				log.Printf("runServer(): recieved a client state change with an error: %s \n", clientStateChange.Error)
				currentState = "ClientError"
			}
			clientStates[clientStateChange.Identifier] = ClientState{
				CurrentState: currentState,
				LastState:    previousState,
			}
			clientStatesMutex.Unlock()
		}
	})()

	if config.UseTls {

		certificates, err := filepath.Glob(config.CaCertificateFilesGlob)
		if err != nil {
			log.Fatal(err)
		}

		caCertPool := x509.NewCertPool()
		for _, filename := range certificates {
			log.Printf("loading certificate %s, clients who have a key signed by this certificat will be allowed to connect", filename)
			caCert, err := ioutil.ReadFile(filename)
			if err != nil {
				log.Fatal(err)
			}
			caCertPool.AppendCertsFromPEM(caCert)
		}

		tlsConfig := &tls.Config{
			ClientCAs:  caCertPool,
			ClientAuth: tls.RequireAndVerifyClientCert,
		}
		tlsConfig.BuildNameToCertificate()

		httpsManagementServer := &http.Server{
			Addr:      fmt.Sprintf(":%d", config.ListenPort),
			TLSConfig: tlsConfig,
			Handler:   &(ManagementHttpHandler{ControlHandler: server}),
		}

		log.Print("runServer(): the server should be running now\n")
		err = httpsManagementServer.ListenAndServeTLS(config.ServerTlsCertificateFile, config.ServerTlsKeyFile)
		panic(err)
	} else {

		log.Print("runServer(): the server should be running now\n")
		err = http.ListenAndServe(fmt.Sprintf(":%d", config.ListenPort), &(ManagementHttpHandler{ControlHandler: server}))
		panic(err)
	}

}

func setListeners(listenerConfigs []ListenerConfig) (int, string) {
	currentListenersThatCanKeepRunning := make([]ListenerConfig, 0)
	newListenersThatHaveToBeAdded := make([]ListenerConfig, 0)

	for _, newListenerConfig := range listenerConfigs {
		clientState, everHeardOfClientBefore := clientStates[newListenerConfig.ClientIdentifier]
		if !everHeardOfClientBefore {
			return http.StatusNotFound, fmt.Sprintf("Client %s Not Found", newListenerConfig.ClientIdentifier)
		}
		if clientState.CurrentState != tunnel.ClientConnected.String() {
			return http.StatusNotFound, fmt.Sprintf("Client %s is not connected it is %s", newListenerConfig.ClientIdentifier, clientState.CurrentState)
		}
	}

	for _, existingListener := range listeners {
		canKeepRunning := false
		for _, newListenerConfig := range listenerConfigs {
			if compareListenerConfigs(existingListener, newListenerConfig) {
				canKeepRunning = true
			}
		}

		if !canKeepRunning {
			listenAddress := net.ParseIP(existingListener.ListenAddress)
			if listenAddress == nil {
				return http.StatusBadRequest, fmt.Sprintf("Bad Request: \"%s\" is not an IP address.", existingListener.ListenAddress)
			}

			server.DeleteAddr(listenAddress, existingListener.ListenPort, existingListener.ListenHostnameGlob)

		} else {
			currentListenersThatCanKeepRunning = append(currentListenersThatCanKeepRunning, existingListener)
		}
	}

	for _, newListenerConfig := range listenerConfigs {
		hasToBeAdded := true
		for _, existingListener := range listeners {
			if compareListenerConfigs(existingListener, newListenerConfig) {
				hasToBeAdded = false
			}
		}

		if hasToBeAdded {
			listenAddress := net.ParseIP(newListenerConfig.ListenAddress)
			//fmt.Printf("str: %s, listenAddress: %s\n\n", newListenerConfig.ListenAddress, listenAddress)
			if listenAddress == nil {
				return http.StatusBadRequest, fmt.Sprintf("Bad Request: \"%s\" is not an IP address.", newListenerConfig.ListenAddress)
			}
			err := server.AddAddr(
				listenAddress,
				newListenerConfig.ListenPort,
				newListenerConfig.ListenHostnameGlob,
				newListenerConfig.ClientIdentifier,
				newListenerConfig.HaProxyProxyProtocol,
				newListenerConfig.BackEndService,
			)

			if err != nil {
				if strings.Contains(err.Error(), "already in use") {
					return http.StatusConflict, fmt.Sprintf("Port Conflict Port %s already in use", listenAddress)
				}

				log.Printf("setListeners(): can't net.Listen(\"tcp\", \"%s\")  because %s \n", listenAddress, err)
				return http.StatusInternalServerError, "Unknown Listening Error"
			}

			newListenersThatHaveToBeAdded = append(newListenersThatHaveToBeAdded, newListenerConfig)
		}
	}

	listeners = append(currentListenersThatCanKeepRunning, newListenersThatHaveToBeAdded...)

	return http.StatusOK, "ok"

}

func compareListenerConfigs(a, b ListenerConfig) bool {
	return (a.ListenPort == b.ListenPort &&
		a.ListenAddress == b.ListenAddress &&
		a.ListenHostnameGlob == b.ListenHostnameGlob &&
		a.BackEndService == b.BackEndService &&
		a.ClientIdentifier == b.ClientIdentifier &&
		a.HaProxyProxyProtocol == b.HaProxyProxyProtocol)
}

func (s *ManagementHttpHandler) ServeHTTP(responseWriter http.ResponseWriter, request *http.Request) {

	switch path.Clean(request.URL.Path) {
	case "/clients":
		if request.Method == "GET" {
			clientStatesMutex.Lock()
			bytes, err := json.Marshal(clientStates)
			clientStatesMutex.Unlock()
			if err != nil {
				http.Error(responseWriter, "500 JSON Marshal Error", http.StatusInternalServerError)
				return
			}
			responseWriter.Header().Set("Content-Type", "application/json")
			responseWriter.Write(bytes)

		} else {
			responseWriter.Header().Set("Allow", "PUT")
			http.Error(responseWriter, "405 Method Not Allowed", http.StatusMethodNotAllowed)
		}
	case "/tunnels":
		if request.Method == "PUT" {
			if request.Header.Get("Content-Type") != "application/json" {
				http.Error(responseWriter, "415 Unsupported Media Type: Content-Type must be application/json", http.StatusUnsupportedMediaType)
			} else {
				bodyBytes, err := ioutil.ReadAll(request.Body)
				if err != nil {
					http.Error(responseWriter, "500 Read Error", http.StatusInternalServerError)
					return
				}
				var listenerConfigs []ListenerConfig
				err = json.Unmarshal(bodyBytes, &listenerConfigs)
				if err != nil {
					http.Error(responseWriter, "422 Unprocessable Entity: Can't Parse JSON", http.StatusUnprocessableEntity)
					return
				}

				statusCode, errorMessage := setListeners(listenerConfigs)

				if statusCode != 200 {
					http.Error(responseWriter, errorMessage, statusCode)
					return
				}

				bytes, err := json.Marshal(listenerConfigs)
				if err != nil {
					http.Error(responseWriter, "500 JSON Marshal Error", http.StatusInternalServerError)
					return
				}

				responseWriter.Header().Set("Content-Type", "application/json")
				responseWriter.Write(bytes)
			}
		} else {
			responseWriter.Header().Set("Allow", "PUT")
			http.Error(responseWriter, "405 Method Not Allowed", http.StatusMethodNotAllowed)
		}
	case "/ping":
		if request.Method == "GET" {
			fmt.Fprint(responseWriter, "pong")
		} else {
			responseWriter.Header().Set("Allow", "GET")
			http.Error(responseWriter, "405 method not allowed", http.StatusMethodNotAllowed)
		}
	default:
		s.ControlHandler.ServeHTTP(responseWriter, request)
	}
}

func getConfigBytes(configFileName *string) []byte {
	if configFileName != nil {
		configBytes, err := ioutil.ReadFile(*configFileName)
		if err != nil {
			log.Printf("getConfigBytes(): can't ioutil.ReadFile(*configFileName) because %s \n", err)
			os.Exit(1)
		}
		return configBytes
	} else {
		log.Printf("getConfigBytes(): configFileName was nil.")
		os.Exit(1)
		return nil
	}
}
