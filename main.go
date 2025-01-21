package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strconv"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

var wsConn *websocket.Conn

func main() {
	port := flag.Int("port", 8080, "http server port")
	flag.Parse()

	http.HandleFunc("/ws", handleWebSocket)

	sdpChan := httpSDPServer(*port)

	offer := webrtc.SessionDescription{}
	decode(<-sdpChan, &offer)
	fmt.Println("")

	peerConnectionConfig := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: []string{"stun:stun.l.google.com:19302"},
			},
		},
	}

	mediaEngine := &webrtc.MediaEngine{}
	if err := mediaEngine.RegisterDefaultCodecs(); err != nil {
		panic(err)
	}

	peerConnectionPublish, err := webrtc.NewAPI(
		webrtc.WithMediaEngine(mediaEngine)).NewPeerConnection(peerConnectionConfig)
	if err != nil {
		panic(err)
	}
	defer func() {
		if cErr := peerConnectionPublish.Close(); cErr != nil {
			fmt.Printf("cannot close peerConnectionPublish: %v\n", cErr)
		}
	}()

	peerConnectionReceive, err := webrtc.NewAPI(
		webrtc.WithMediaEngine(mediaEngine)).NewPeerConnection(peerConnectionConfig)
	if err != nil {
		panic(err)
	}
	defer func() {
		if cErr := peerConnectionReceive.Close(); cErr != nil {
			fmt.Printf("cannot close peerConnectionReceive: %v\n", cErr)
		}
	}()

	localTrackChan := make(chan *webrtc.TrackLocalStaticRTP)

	peerConnectionPublish.OnTrack(func(remoteTrack *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		localTrack, newTrackErr := webrtc.NewTrackLocalStaticRTP(remoteTrack.Codec().RTPCodecCapability, "video", "pion")
		if newTrackErr != nil {
			panic(newTrackErr)
		}
		localTrackChan <- localTrack

		rtpBuf := make([]byte, 1400)
		for {
			i, _, readErr := remoteTrack.Read(rtpBuf)
			if readErr != nil {
				panic(readErr)
			}

			if _, err = localTrack.Write(rtpBuf[:i]); err != nil && !errors.Is(err, io.ErrClosedPipe) {
				panic(err)
			}
		}
	})

	err = peerConnectionPublish.SetRemoteDescription(offer)
	if err != nil {
		panic(err)
	}

	answer, err := peerConnectionPublish.CreateAnswer(nil)
	if err != nil {
		panic(err)
	}

	gatherComplete := webrtc.GatheringCompletePromise(peerConnectionPublish)

	err = peerConnectionPublish.SetLocalDescription(answer)
	if err != nil {
		panic(err)
	}

	<-gatherComplete

	sdpBase64Sender := encode(peerConnectionPublish.LocalDescription())

	if wsConn != nil {
		sendSDP(wsConn, sdpBase64Sender, "sdp-answer-sender")
	}

	localTrack := <-localTrackChan
	for {
		fmt.Println("Curl an base64 SDP to start sendonly peer connection")

		recvOnlyOffer := webrtc.SessionDescription{}
		decode(<-sdpChan, &recvOnlyOffer)

		peerConnectionReceive, err := webrtc.NewPeerConnection(peerConnectionConfig)
		if err != nil {
			panic(err)
		}

		rtpSender, err := peerConnectionReceive.AddTrack(localTrack)
		if err != nil {
			panic(err)
		}

		go func() {
			rtcpBuf := make([]byte, 1500)
			for {
				if _, _, rtcpErr := rtpSender.Read(rtcpBuf); rtcpErr != nil {
					return
				}
			}
		}()

		err = peerConnectionReceive.SetRemoteDescription(recvOnlyOffer)
		if err != nil {
			panic(err)
		}

		answer, err := peerConnectionReceive.CreateAnswer(nil)
		if err != nil {
			panic(err)
		}

		gatherComplete = webrtc.GatheringCompletePromise(peerConnectionReceive)

		err = peerConnectionReceive.SetLocalDescription(answer)
		if err != nil {
			panic(err)
		}

		<-gatherComplete

		sdpBase64Receiver := encode(peerConnectionReceive.LocalDescription())

		if wsConn != nil {
			sendSDP(wsConn, sdpBase64Receiver, "sdp-answer-reciever")
		}

	}
}

func encode(obj *webrtc.SessionDescription) string {
	b, err := json.Marshal(obj)
	if err != nil {
		panic(err)
	}

	return base64.StdEncoding.EncodeToString(b)
}

func decode(in string, obj *webrtc.SessionDescription) {
	b, err := base64.StdEncoding.DecodeString(in)
	if err != nil {
		panic(err)
	}

	if err = json.Unmarshal(b, obj); err != nil {
		panic(err)
	}
}

func httpSDPServer(port int) chan string {
	sdpChan := make(chan string)
	http.HandleFunc("/", func(res http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		fmt.Fprintf(res, "done")
		sdpChan <- string(body)
	})

	go func() {
		panic(http.ListenAndServe(":"+strconv.Itoa(port), nil))
	}()

	return sdpChan
}

type Message struct {
	Type string `json:"type"`
	Data string `json:"data"`
}

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		fmt.Println("WebSocket upgrade failed:", err)
		return
	}
	defer conn.Close()

	wsConn = conn

	for {
		messageType, p, err := conn.ReadMessage()
		if err != nil {
			fmt.Println("Error reading WebSocket message:", err)
			break
		}

		if messageType == websocket.TextMessage {
			var msg Message
			if err := json.Unmarshal(p, &msg); err != nil {
				fmt.Println("Error unmarshalling WebSocket message:", err)
				continue
			}

			if msg.Type == "sender-sdp-offer" || msg.Type == "reciever-sdp-offer" {
				cmd := exec.Command("curl", "localhost:8080", "-d", msg.Data)
				_, err := cmd.CombinedOutput()
				if err != nil {
					fmt.Printf("Error executing curl: %v\n", err)
				}
			}

		}
	}
}

func sendSDP(conn *websocket.Conn, sdp string, sdpType string) {
	message := Message{
		Type: sdpType,
		Data: sdp,
	}

	messageBytes, err := json.Marshal(message)
	if err != nil {
		fmt.Println("Error marshalling WebSocket message:", err)
		return
	}

	err = conn.WriteMessage(websocket.TextMessage, messageBytes)
	if err != nil {
		fmt.Println("Error sending WebSocket message:", err)
	}
}
