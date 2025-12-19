package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
)

type TransmissionConnectionConfig struct {
	url      string
	username string
	password string
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

func main() {
	url := flag.String("url", "http://localhost:9091/transmission/rpc", "URL of transmission")
	username := flag.String("username", "", "Transmission RPC username")
	password := flag.String("password", "", "Transmission RPC password")
	flag.Parse()

	transmissionConnectionConfig := TransmissionConnectionConfig{
		url:      *url,
		username: *username,
		password: *password,
	}

	transmissionSessionId, _ := getTransmissionSessionId(transmissionConnectionConfig)
	peerPort, err := getPeerPort(transmissionConnectionConfig, transmissionSessionId)
	if err != nil {
		fmt.Println("Error:", err)
	}

	fmt.Println("Peer port:", peerPort)
	setPeerPort(transmissionConnectionConfig, transmissionSessionId, 6666)
	// ticker := time.NewTicker(5 * time.Second)
	// quit := make(chan struct{})
	// go func() {
	// 	for {
	// 		select {
	// 		case <-ticker.C:
	// 			fmt.Println(time.Now())
	// 		case <-quit:
	// 			ticker.Stop()
	// 			return
	// 		}
	// 	}
	// }()

	// input := bufio.NewScanner(os.Stdin)
	// input.Scan()
	// close(quit)
}

func getTransmissionSessionId(transmissionConnectionConfig TransmissionConnectionConfig) (sessionId string, err error) {
	req, _ := http.NewRequest("POST", transmissionConnectionConfig.url, nil)
	req.SetBasicAuth(transmissionConnectionConfig.username, transmissionConnectionConfig.password)

	httpClient := &http.Client{}
	resp, err := httpClient.Do(req)
	if err != nil {
		return
	}
	resp.Body.Close()

	sessionId = resp.Header.Get("X-Transmission-Session-Id")
	return
}

func setPeerPort(transmissionConnectionConfig TransmissionConnectionConfig, transmissionSessionId string, port int) error {
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

	body, _ := io.ReadAll(httpResp.Body)
	respData := new(TransmissionSetSessionResponse)
	err = json.Unmarshal(body, &respData)

	if respData.Result != "success" {
		return fmt.Errorf("Error setting peer port, transmission responded: %s", string(body))
	}
	return nil
}

func getPeerPort(transmissionConnectionConfig TransmissionConnectionConfig, transmissionSessionId string) (peerPort int, err error) {
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
