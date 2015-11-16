package server // import "bitbucket.org/stendec/frankerfacez/socketserver/internal/server"

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gorilla/websocket"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SuccessCommand is a Reply Command to indicate success in reply to a C2S Command.
const SuccessCommand Command = "ok"

// ErrorCommand is a Reply Command to indicate that a C2S Command failed.
const ErrorCommand Command = "error"

// HelloCommand is a C2S Command.
// HelloCommand must be the Command of the first ClientMessage sent during a connection.
// Sending any other command will result in a CloseFirstMessageNotHello.
const HelloCommand Command = "hello"

// AuthorizeCommand is a S2C Command sent as part of Twitch username validation.
const AuthorizeCommand Command = "do_authorize"

// AsyncResponseCommand is a pseudo-Reply Command.
// It indicates that the Reply Command to the client's C2S Command will be delivered
// on a goroutine over the ClientInfo.MessageChannel and should not be delivered immediately.
const AsyncResponseCommand Command = "_async"

// ResponseSuccess is a Reply ClientMessage with the MessageID not yet filled out.
var ResponseSuccess = ClientMessage{Command: SuccessCommand}

// Configuration is the active ConfigFile.
var Configuration *ConfigFile

// SetupServerAndHandle starts all background goroutines and registers HTTP listeners on the given ServeMux.
// Essentially, this function completely preps the server for a http.ListenAndServe call.
// (Uses http.DefaultServeMux if `serveMux` is nil.)
func SetupServerAndHandle(config *ConfigFile, serveMux *http.ServeMux) {
	Configuration = config

	setupBackend(config)

	if serveMux == nil {
		serveMux = http.DefaultServeMux
	}

	bannerBytes, err := ioutil.ReadFile("index.html")
	if err != nil {
		log.Fatalln("Could not open index.html:", err)
	}
	BannerHTML = bannerBytes

	serveMux.HandleFunc("/", ServeWebsocketOrCatbag)
	serveMux.HandleFunc("/drop_backlog", HBackendDropBacklog)
	serveMux.HandleFunc("/uncached_pub", HBackendPublishRequest)
	serveMux.HandleFunc("/cached_pub", HBackendUpdateAndPublish)

	announceForm, err := SealRequest(url.Values{
		"startup": []string{"1"},
	})
	if err != nil {
		log.Fatalln("Unable to seal requests:", err)
	}
	resp, err := backendHTTPClient.PostForm(announceStartupURL, announceForm)
	if err != nil {
		log.Println(err)
	} else {
		resp.Body.Close()
	}

	go authorizationJanitor()
	go backlogJanitor()
	go bunchCacheJanitor()
	go pubsubJanitor()
	go sendAggregateData()

	go ircConnection()
}

// SocketUpgrader is the websocket.Upgrader currently in use.
var SocketUpgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return r.Header.Get("Origin") == "http://www.twitch.tv"
	},
}

// BannerHTML is the content served to web browsers viewing the socket server website.
// Memes go here.
var BannerHTML []byte

func ServeWebsocketOrCatbag(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Connection") == "Upgrade" {
		conn, err := SocketUpgrader.Upgrade(w, r, nil)
		if err != nil {
			fmt.Fprintf(w, "error: %v", err)
			return
		}
		HandleSocketConnection(conn)

		return
	} else {
		w.Write(BannerHTML)
	}
}

// Errors that get returned to the client.
var ProtocolError error = errors.New("FFZ Socket protocol error.")
var ProtocolErrorNegativeID error = errors.New("FFZ Socket protocol error: negative or zero message ID.")
var ExpectedSingleString = errors.New("Error: Expected single string as arguments.")
var ExpectedSingleInt = errors.New("Error: Expected single integer as arguments.")
var ExpectedTwoStrings = errors.New("Error: Expected array of string, string as arguments.")
var ExpectedStringAndInt = errors.New("Error: Expected array of string, int as arguments.")
var ExpectedStringAndBool = errors.New("Error: Expected array of string, bool as arguments.")
var ExpectedStringAndIntGotFloat = errors.New("Error: Second argument was a float, expected an integer.")

var CloseGotBinaryMessage = websocket.CloseError{Code: websocket.CloseUnsupportedData, Text: "got binary packet"}
var CloseTimedOut = websocket.CloseError{Code: websocket.CloseNoStatusReceived, Text: "no ping replies for 5 minutes"}
var CloseFirstMessageNotHello = websocket.CloseError{
	Text: "Error - the first message sent must be a 'hello'",
	Code: websocket.ClosePolicyViolation,
}

// Handle a new websocket connection from a FFZ client.
// This runs in a goroutine started by net/http.
func HandleSocketConnection(conn *websocket.Conn) {
	// websocket.Conn is a ReadWriteCloser

	log.Println("Got socket connection from", conn.RemoteAddr())

	var _closer sync.Once
	closer := func() {
		_closer.Do(func() {
			conn.Close()
		})
	}

	// Close the connection when we're done.
	defer closer()

	_clientChan := make(chan ClientMessage)
	_serverMessageChan := make(chan ClientMessage)
	_errorChan := make(chan error)
	stoppedChan := make(chan struct{})

	var client ClientInfo
	client.MessageChannel = _serverMessageChan
	client.RemoteAddr = conn.RemoteAddr()
	client.MsgChannelIsDone = stoppedChan

	// Launch receiver goroutine
	go func(errorChan chan<- error, clientChan chan<- ClientMessage, stoppedChan <-chan struct{}) {
		var msg ClientMessage
		var messageType int
		var packet []byte
		var err error
		for ; err == nil; messageType, packet, err = conn.ReadMessage() {
			if messageType == websocket.BinaryMessage {
				err = &CloseGotBinaryMessage
				break
			}
			if messageType == websocket.CloseMessage {
				err = io.EOF
				break
			}

			UnmarshalClientMessage(packet, messageType, &msg)
			if msg.MessageID == 0 {
				continue
			}
			select {
			case clientChan <- msg:
			case <-stoppedChan:
				close(errorChan)
				close(clientChan)
				return
			}
		}

		_, isClose := err.(*websocket.CloseError)
		if err != io.EOF && !isClose {
			log.Println("Error while reading from client:", err)
		}
		select {
		case errorChan <- err:
		case <-stoppedChan:
		}
		close(errorChan)
		close(clientChan)
		// exit
	}(_errorChan, _clientChan, stoppedChan)

	conn.SetPongHandler(func(pongBody string) error {
		client.pingCount = 0
		return nil
	})

	var errorChan <-chan error = _errorChan
	var clientChan <-chan ClientMessage = _clientChan
	var serverMessageChan <-chan ClientMessage = _serverMessageChan

	// All set up, now enter the work loop

RunLoop:
	for {
		select {
		case err := <-errorChan:
			if err == io.EOF {
				conn.Close() // no need to send a close frame :)
				break RunLoop
			} else if closeMsg, isClose := err.(*websocket.CloseError); isClose {
				CloseConnection(conn, closeMsg)
			} else {
				CloseConnection(conn, &websocket.CloseError{
					Code: websocket.CloseInternalServerErr,
					Text: err.Error(),
				})
			}

			break RunLoop

		case msg := <-clientChan:
			if client.Version == "" && msg.Command != HelloCommand {
				log.Println("error - first message wasn't hello from", conn.RemoteAddr(), "-", msg)
				CloseConnection(conn, &CloseFirstMessageNotHello)
				break RunLoop
			}

			HandleCommand(conn, &client, msg)

		case smsg := <-serverMessageChan:
			SendMessage(conn, smsg)

		case <-time.After(1 * time.Minute):
			client.pingCount++
			if client.pingCount == 5 {
				CloseConnection(conn, &CloseTimedOut)
				break RunLoop
			} else {
				conn.WriteControl(websocket.PingMessage, []byte(strconv.FormatInt(time.Now().Unix(), 10)), getDeadline())
			}
		}
	}

	// Exit

	// Launch message draining goroutine - we aren't out of the pub/sub records
	go func() {
		for _ = range _serverMessageChan {
		}
	}()

	close(stoppedChan)

	// Stop getting messages...
	UnsubscribeAll(&client)

	// Wait for pending jobs to finish...
	client.MsgChannelKeepalive.Wait()
	client.MessageChannel = nil

	// And done.
	// Close the channel so the draining goroutine can finish, too.
	close(_serverMessageChan)

	log.Println("End socket connection from", conn.RemoteAddr())
}

func getDeadline() time.Time {
	return time.Now().Add(1 * time.Minute)
}

func CallHandler(handler CommandHandler, conn *websocket.Conn, client *ClientInfo, cmsg ClientMessage) (rmsg ClientMessage, err error) {
	defer func() {
		if r := recover(); r != nil {
			var ok bool
			fmt.Print("[!] Error executing command", cmsg.Command, "--", r)
			err, ok = r.(error)
			if !ok {
				err = fmt.Errorf("command handler: %v", r)
			}
		}
	}()
	return handler(conn, client, cmsg)
}

func CloseConnection(conn *websocket.Conn, closeMsg *websocket.CloseError) {
	if closeMsg != &CloseFirstMessageNotHello {
		log.Println("Terminating connection with", conn.RemoteAddr(), "-", closeMsg.Text)
	}
	conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(closeMsg.Code, closeMsg.Text), getDeadline())
	conn.Close()
}

// SendMessage sends a ClientMessage over the websocket connection with a timeout.
// If marshalling the ClientMessage fails, this function will panic.
func SendMessage(conn *websocket.Conn, msg ClientMessage) {
	messageType, packet, err := MarshalClientMessage(msg)
	if err != nil {
		panic(fmt.Sprintf("failed to marshal: %v %v", err, msg))
	}
	conn.SetWriteDeadline(getDeadline())
	conn.WriteMessage(messageType, packet)
}

// UnmarshalClientMessage unpacks websocket TextMessage into a ClientMessage provided in the `v` parameter.
func UnmarshalClientMessage(data []byte, payloadType int, v interface{}) (err error) {
	var spaceIdx int

	out := v.(*ClientMessage)
	dataStr := string(data)

	// Message ID
	spaceIdx = strings.IndexRune(dataStr, ' ')
	if spaceIdx == -1 {
		return ProtocolError
	}
	messageID, err := strconv.Atoi(dataStr[:spaceIdx])
	if messageID < -1 || messageID == 0 {
		return ProtocolErrorNegativeID
	}

	out.MessageID = messageID
	dataStr = dataStr[spaceIdx+1:]

	spaceIdx = strings.IndexRune(dataStr, ' ')
	if spaceIdx == -1 {
		out.Command = Command(dataStr)
		out.Arguments = nil
		return nil
	} else {
		out.Command = Command(dataStr[:spaceIdx])
	}
	dataStr = dataStr[spaceIdx+1:]
	argumentsJSON := dataStr
	out.origArguments = argumentsJSON
	err = out.parseOrigArguments()
	if err != nil {
		return
	}
	return nil
}

func (cm *ClientMessage) parseOrigArguments() error {
	err := json.Unmarshal([]byte(cm.origArguments), &cm.Arguments)
	if err != nil {
		return err
	}
	return nil
}

func MarshalClientMessage(clientMessage interface{}) (payloadType int, data []byte, err error) {
	var msg ClientMessage
	var ok bool
	msg, ok = clientMessage.(ClientMessage)
	if !ok {
		pMsg, ok := clientMessage.(*ClientMessage)
		if !ok {
			panic("MarshalClientMessage: argument needs to be a ClientMessage")
		}
		msg = *pMsg
	}
	var dataStr string

	if msg.Command == "" && msg.MessageID == 0 {
		panic("MarshalClientMessage: attempt to send an empty ClientMessage")
	}

	if msg.Command == "" {
		msg.Command = SuccessCommand
	}
	if msg.MessageID == 0 {
		msg.MessageID = -1
	}

	if msg.Arguments != nil {
		argBytes, err := json.Marshal(msg.Arguments)
		if err != nil {
			return 0, nil, err
		}

		dataStr = fmt.Sprintf("%d %s %s", msg.MessageID, msg.Command, string(argBytes))
	} else {
		dataStr = fmt.Sprintf("%d %s", msg.MessageID, msg.Command)
	}

	return websocket.TextMessage, []byte(dataStr), nil
}

// Command handlers should use this to construct responses.
func SuccessMessageFromString(arguments string) ClientMessage {
	cm := ClientMessage{
		MessageID:     -1, // filled by the select loop
		Command:       SuccessCommand,
		origArguments: arguments,
	}
	cm.parseOrigArguments()
	return cm
}

// Convenience method: Parse the arguments of the ClientMessage as a single string.
func (cm *ClientMessage) ArgumentsAsString() (string1 string, err error) {
	var ok bool
	string1, ok = cm.Arguments.(string)
	if !ok {
		err = ExpectedSingleString
		return
	} else {
		return string1, nil
	}
}

// Convenience method: Parse the arguments of the ClientMessage as a single int.
func (cm *ClientMessage) ArgumentsAsInt() (int1 int64, err error) {
	var ok bool
	var num float64
	num, ok = cm.Arguments.(float64)
	if !ok {
		err = ExpectedSingleInt
		return
	} else {
		int1 = int64(num)
		return int1, nil
	}
}

// Convenience method: Parse the arguments of the ClientMessage as an array of two strings.
func (cm *ClientMessage) ArgumentsAsTwoStrings() (string1, string2 string, err error) {
	var ok bool
	var ary []interface{}
	ary, ok = cm.Arguments.([]interface{})
	if !ok {
		err = ExpectedTwoStrings
		return
	} else {
		if len(ary) != 2 {
			err = ExpectedTwoStrings
			return
		}
		string1, ok = ary[0].(string)
		if !ok {
			err = ExpectedTwoStrings
			return
		}
		// clientID can be null
		if ary[1] == nil {
			return string1, "", nil
		}
		string2, ok = ary[1].(string)
		if !ok {
			err = ExpectedTwoStrings
			return
		}
		return string1, string2, nil
	}
}

// Convenience method: Parse the arguments of the ClientMessage as an array of a string and an int.
func (cm *ClientMessage) ArgumentsAsStringAndInt() (string1 string, int int64, err error) {
	var ok bool
	var ary []interface{}
	ary, ok = cm.Arguments.([]interface{})
	if !ok {
		err = ExpectedStringAndInt
		return
	} else {
		if len(ary) != 2 {
			err = ExpectedStringAndInt
			return
		}
		string1, ok = ary[0].(string)
		if !ok {
			err = ExpectedStringAndInt
			return
		}
		var num float64
		num, ok = ary[1].(float64)
		if !ok {
			err = ExpectedStringAndInt
			return
		}
		int = int64(num)
		if float64(int) != num {
			err = ExpectedStringAndIntGotFloat
			return
		}
		return string1, int, nil
	}
}

// Convenience method: Parse the arguments of the ClientMessage as an array of a string and an int.
func (cm *ClientMessage) ArgumentsAsStringAndBool() (str string, flag bool, err error) {
	var ok bool
	var ary []interface{}
	ary, ok = cm.Arguments.([]interface{})
	if !ok {
		err = ExpectedStringAndBool
		return
	} else {
		if len(ary) != 2 {
			err = ExpectedStringAndBool
			return
		}
		str, ok = ary[0].(string)
		if !ok {
			err = ExpectedStringAndBool
			return
		}
		flag, ok = ary[1].(bool)
		if !ok {
			err = ExpectedStringAndBool
			return
		}
		return str, flag, nil
	}
}
