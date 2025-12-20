package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"time"

	natpmp "github.com/jackpal/go-nat-pmp"
)

const natpmpPortMappingLifespanSeconds = 60 // by rfc6886 recommends 2 hours but protonvpn documentation seems to hint 60 seconds
const natpmpPortMappingRefreshIntervalSeconds = 45

var logger *slog.Logger

type TransmissionConnectionConfig struct {
	url      string
	username string
	password string
}

type TransmissionConnection struct {
	transmissionConnectionConfig TransmissionConnectionConfig
	transmissionSessionId        string
}

type TransmissionGetSessionResponse struct {
	Arguments TransmissionGetSessionResponseArguments `json:"arguments"`
}

type TransmissionGetSessionResponseArguments struct {
	PeerPort int `json:"peer-port"`
}

type TransmissionSetSessionResponse struct {
	Result string `json:"result"`
}

type TransmissionRpcError struct {
	StatusCode int
	SessionId  string
	Err        error
}

func (e *TransmissionRpcError) Error() string {
	return e.Err.Error()
}

func main() {
	url := flag.String("url", "http://localhost:9091/transmission/rpc", "URL of transmission")
	username := flag.String("username", "", "Transmission RPC username")
	password := flag.String("password", "", "Transmission RPC password")
	natpmpGateway := flag.String("natpmp-gateway", "", "The IP of the gateway to request port mappings from")
	verbose := flag.Bool("verbose", false, "Enable verbose output")
	flag.Parse()

	var logOutputLevel slog.Level
	if *verbose {
		logOutputLevel = slog.LevelDebug
	} else {
		logOutputLevel = slog.LevelInfo
	}
	logger = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: logOutputLevel,
	}))
	logger.Info("Starting")

	transmissionConnectionConfig := TransmissionConnectionConfig{
		url:      *url,
		username: *username,
		password: *password,
	}

	transmissionConnection := TransmissionConnection{
		transmissionConnectionConfig: transmissionConnectionConfig,
	}

	peerPort, err := getPeerPort(transmissionConnection)
	if err != nil {
		logger.Error("Error getting configured peer port from transmission", "error", err)
		os.Exit(1)
	}

	logger.Debug("Transmission's configured peer port is", "port", peerPort)

	natpmpGatewayIp, err := net.ResolveIPAddr("ip", *natpmpGateway)
	if err != nil {
		logger.Error("Invalid argument: natpmp gateway IP address is not a valid IP address", "natpmpGateway", *natpmpGateway)
		os.Exit(1)
	}

	natpmpClient := natpmp.NewClient(natpmpGatewayIp.IP)
	externalAddressResponse, err := natpmpClient.GetExternalAddress()
	logger.Debug("External IP address", "externalIpAddress", externalAddressResponse.ExternalIPAddress)

	mappedPort, err := updateTransmissionPortMapping(transmissionConnection, *natpmpClient, uint(peerPort))
	if err != nil {
		logger.Error("Failed to map and configure port", "error", err)
		os.Exit(1)
	}

	logger.Debug("Successfully mapped", "port", mappedPort)

	var waitGroup sync.WaitGroup
	quit := make(chan struct{})
	sigInteruptChannel := make(chan os.Signal, 1)
	signal.Notify(sigInteruptChannel, os.Interrupt)
	go func() {
		<-sigInteruptChannel
		logger.Info("Shutting down...")
		close(quit)
	}()

	ticker := time.NewTicker(natpmpPortMappingRefreshIntervalSeconds * time.Second)
	waitGroup.Add(1)
	go func() {
		defer waitGroup.Done()

		for {
			select {
			case <-ticker.C:
				logger.Debug("Updating port mapping...")
				mappedPort, err = updateTransmissionPortMapping(transmissionConnection, *natpmpClient, uint(peerPort))
				if err != nil {
					logger.Error("Failed to update port mapping", "error", err)
				} else {
					if mappedPort == uint(peerPort) {
						logger.Debug("Refreshed port mapping", "port", mappedPort)
					} else {
						logger.Info("Port mapping changed", "previousPort", peerPort, "newPort", mappedPort)
						setPeerPort(transmissionConnection, int(mappedPort))
					}
					peerPort = int(mappedPort)
				}
			case <-quit:
				ticker.Stop()
				return
			}
		}
	}()

	waitGroup.Wait()
}

func updateTransmissionPortMapping(transmissionConnection TransmissionConnection, natpmpClient natpmp.Client, existingPeerPort uint) (mappedPort uint, err error) {
	_mappedPort, err := createPortMapping(natpmpClient, existingPeerPort)
	if err != nil {
		return
	}

	transmissionPeerPort, err := getPeerPort(transmissionConnection)
	if err != nil {
		return
	}

	if transmissionPeerPort != int(_mappedPort) {
		logger.Debug("Updating transmission peer port...")
		err = setPeerPort(transmissionConnection, int(mappedPort))
		if err != nil {
			return
		}
	}

	mappedPort = _mappedPort
	return
}

func createPortMapping(natpmpClient natpmp.Client, desiredPort uint) (mappedPort uint, err error) {
	port := int(desiredPort)

	for i := 0; i < 3; i++ {
		logger.Debug("Attempting to map port", "desiredPort", port)
		result, err := natpmpClient.AddPortMapping("tcp", 0, port, natpmpPortMappingLifespanSeconds)
		if err != nil {
			logger.Error("Failed to create port mapping", "desiredPort", port, "error", err)
			continue
		} else {
			logger.Debug("Successfully mapped port", "externalPort", result.MappedExternalPort, "internalPort", result.InternalPort)
		}

		if result.InternalPort > 0 && result.MappedExternalPort != result.InternalPort {
			port = int(result.MappedExternalPort)
			natpmpClient.AddPortMapping("tcp", int(result.InternalPort), 0, 0)
		} else {
			mappedPort = uint(result.MappedExternalPort)
			break
		}
	}

	if mappedPort == 0 {
		err = fmt.Errorf("Failed to map an external port to an internal port with the same port number")
		return
	}

	addUdpPortMappingResult, err := natpmpClient.AddPortMapping("udp", 0, int(mappedPort), natpmpPortMappingLifespanSeconds)
	if err != nil {
		return
	}

	if addUdpPortMappingResult.InternalPort > 0 && addUdpPortMappingResult.MappedExternalPort != addUdpPortMappingResult.InternalPort {
		err = fmt.Errorf("Failed to create a UDP port mapping with the same TCP port")
	}

	return
}

func setPeerPort(transmissionConnection TransmissionConnection, port int) (err error) {
	for i := 0; i < 3; i++ {
		err = _setPeerPort(transmissionConnection.transmissionConnectionConfig, transmissionConnection.transmissionSessionId, port)
		if err != nil {
			var transmissionRpcError *TransmissionRpcError
			if errors.As(err, &transmissionRpcError) {
				transmissionConnection.transmissionSessionId = transmissionRpcError.SessionId
				continue
			}
		} else {
			err = nil
		}
	}

	return
}

func _setPeerPort(transmissionConnectionConfig TransmissionConnectionConfig, transmissionSessionId string, port int) error {
	requestStr := fmt.Sprintf("{\"method\": \"session-set\", \"arguments\": {\"peer-port\": %d}}", port)
	requestData := []byte(requestStr)

	req, _ := http.NewRequest("POST", transmissionConnectionConfig.url, bytes.NewBuffer(requestData))
	req.SetBasicAuth(transmissionConnectionConfig.username, transmissionConnectionConfig.password)
	req.Header.Set("X-Transmission-Session-Id", transmissionSessionId)

	client := &http.Client{}
	httpResp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode == 409 {
		return &TransmissionRpcError{
			StatusCode: httpResp.StatusCode,
			SessionId:  getTransmissionSessionIdFromResponse(httpResp),
			Err:        fmt.Errorf("Received HTTP %d from Transmission", httpResp.StatusCode),
		}
	}

	body, _ := io.ReadAll(httpResp.Body)
	respData := new(TransmissionSetSessionResponse)
	err = json.Unmarshal(body, &respData)

	if respData.Result != "success" {
		return fmt.Errorf("Error setting peer port, transmission responded: %s", string(body))
	}
	return nil
}

func getPeerPort(transmissionConnection TransmissionConnection) (peerPort int, err error) {
	for i := 0; i < 3; i++ {
		peerPort, err = _getPeerPort(transmissionConnection.transmissionConnectionConfig, transmissionConnection.transmissionSessionId)
		if err != nil {
			var transmissionRpcError *TransmissionRpcError
			if errors.As(err, &transmissionRpcError) {
				transmissionConnection.transmissionSessionId = transmissionRpcError.SessionId
				continue
			}
		} else {
			err = nil
		}
	}

	return
}

func _getPeerPort(transmissionConnectionConfig TransmissionConnectionConfig, transmissionSessionId string) (peerPort int, err error) {
	requestData := []byte("{\"method\": \"session-get\"}")

	req, _ := http.NewRequest("POST", transmissionConnectionConfig.url, bytes.NewBuffer(requestData))
	req.SetBasicAuth(transmissionConnectionConfig.username, transmissionConnectionConfig.password)
	req.Header.Set("X-Transmission-Session-Id", transmissionSessionId)

	httpClient := &http.Client{}
	resp, err := httpClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == 409 {
		err = &TransmissionRpcError{
			StatusCode: resp.StatusCode,
			SessionId:  getTransmissionSessionIdFromResponse(resp),
			Err:        fmt.Errorf("Received HTTP %d from Transmission", resp.StatusCode),
		}
		return
	}

	if resp.StatusCode != 200 {
		err = fmt.Errorf("Transmission returned status code %d", resp.StatusCode)
		return
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return
	}

	responseStruct := new(TransmissionGetSessionResponse)
	err = json.Unmarshal(body, &responseStruct)
	if err != nil {
		return
	}

	peerPort = responseStruct.Arguments.PeerPort
	return
}

func getTransmissionSessionIdFromResponse(httpResponse *http.Response) string {
	return httpResponse.Header.Get("X-Transmission-Session-Id")
}
