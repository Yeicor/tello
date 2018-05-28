// network.go

// Copyright (C) 2018  Steve Merrony

// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.

// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package tello

import (
	"bytes"
	"errors"
	"log"
	"net"
	"strconv"
	"sync"
	"time"
)

const (
	defaultTelloAddr        = "192.168.10.1"
	defaultTelloControlPort = 8889
	defaultLocalControlPort = 8800
	defaultTelloVideoPort   = 6038
	defaultLocalVideoPort   = 8801
)

const keepAlivePeriodMs = 50

// Tello holds the current state of a connection to a Tello drone
type Tello struct {
	ctrlMu                         sync.RWMutex // this mutex protects the control fields
	ctrlConn, videoConn            *net.UDPConn
	ctrlStopChan, videoStopChan    chan bool
	ctrlConnecting, ctrlConnected  bool
	ctrlSeq                        uint16
	ctrlRx, ctrlRy, ctrlLx, ctrlLy float64
	ctrlThrottle                   float64
	stickChan                      chan StickMessage // this will receive stick updates from the user
	fdMu                           sync.RWMutex      // this mutex protects the flight data fields
	fd                             FlightData        // our private amalgamated store of the latest data
	fdStreaming                    bool              // are we currently sending FlightData out?
}

// ControlConnect attempts to connect to a Tello at the provided network addr.
// It then starts listening for responses on the control channel and waits for the Tello to respond
func (tello *Tello) ControlConnect(udpAddr string, droneUDPPort int, localUDPPort int) (stkChan <-chan StickMessage, err error) {
	// first check that we are not already connected or connecting
	tello.ctrlMu.RLock()
	if tello.ctrlConnected {
		tello.ctrlMu.RUnlock()
		return nil, errors.New("Tello already connected")
	}
	if tello.ctrlConnecting {
		tello.ctrlMu.RUnlock()
		return nil, errors.New("Tello connection attempt already in progress")
	}
	tello.ctrlMu.RUnlock()

	droneAddr, err := net.ResolveUDPAddr("udp", udpAddr+":"+strconv.Itoa(droneUDPPort))
	if err != nil {
		return nil, err
	}
	localAddr, err := net.ResolveUDPAddr("udp", ":"+strconv.Itoa(localUDPPort))
	if err != nil {
		return nil, err
	}
	tello.ctrlMu.Lock()
	tello.ctrlConn, err = net.DialUDP("udp", localAddr, droneAddr)
	tello.ctrlMu.Unlock()
	if err != nil {
		return nil, err
	}

	// start the control listener Goroutine
	tello.ctrlMu.Lock()
	tello.ctrlStopChan = make(chan bool, 2)
	tello.ctrlMu.Unlock()
	go tello.controlResponseListener()

	// say hello to the Tello
	tello.sendConnectRequest(defaultTelloVideoPort)

	// wait up to 3 seconds for the Tello to respond
	for t := 0; t < 10; t++ {
		tello.ctrlMu.RLock()
		if tello.ctrlConnected {
			tello.ctrlMu.RUnlock()
			break
		}
		tello.ctrlMu.RUnlock()
		time.Sleep(333 * time.Millisecond)
	}
	tello.ctrlMu.RLock()
	if !tello.ctrlConnected {
		tello.ctrlMu.RUnlock()
		return nil, errors.New("Timeout waiting for response to connection request from Tello")
	}
	tello.ctrlMu.RUnlock()

	// start the keepalive transmitter
	go tello.keepAlive()

	// start the stick listener
	tello.stickChan = make(chan StickMessage, 10)
	go tello.stickListener()

	return tello.stickChan, nil
}

// ControlConnectDefault attempts to connect to a Tello on the default network addresses.
// It then starts listening for responses on the control channel and waits for the Tello to respond
func (tello *Tello) ControlConnectDefault() (stkChan <-chan StickMessage, err error) {
	return tello.ControlConnect(defaultTelloAddr, defaultTelloControlPort, defaultLocalControlPort)
}

// ControlDisconnect stops the control channel listener and closes the connection to a Tello
func (tello *Tello) ControlDisconnect() {
	// TODO should we tell the Tello we are disconnecting?
	tello.ctrlStopChan <- true
	tello.ctrlConn.Close()
	tello.ctrlConnected = false
}

// VideoConnect attempts to connect to a Tello video channel at the provided adrr and starts a listener
func (tello *Tello) VideoConnect(udpAddr string, droneUDPPort int, localUDPPort int) (err error) {
	droneAddr, err := net.ResolveUDPAddr("udp", udpAddr+":"+strconv.Itoa(droneUDPPort))
	if err != nil {
		return err
	}
	// localAddr, err := net.ResolveUDPAddr("udp", ":"+strconv.Itoa(localUDPPort))
	// if err != nil {
	// 	return err
	// }
	tello.videoConn, err = net.ListenUDP("udp", droneAddr)
	if err != nil {
		return err
	}
	tello.videoStopChan = make(chan bool, 2)
	go tello.videoResponseListener()
	return nil
}

// VideoConnectDefault attempts to connect to a Tello video channel using default addresses, then starts a listener
func (tello *Tello) VideoConnectDefault() (err error) {
	return tello.VideoConnect(defaultTelloAddr, defaultTelloVideoPort, defaultLocalVideoPort)
}

// VideoDisconnect closes the connecttion to the video channel
func (tello *Tello) VideoDisconnect() {
	// TODO Should we tell the Tello we are stopping video listening?
	tello.videoStopChan <- true
	tello.videoConn.Close()
}

// GetFlightData returns the current known state of the Tello
func (tello *Tello) GetFlightData() FlightData {
	tello.fdMu.RLock()
	rfd := tello.fd
	tello.fdMu.RUnlock()
	return rfd
}

// StreamFlightData starts a Goroutine which sends FlightData to a channel
// If asAvailable is true then updates are sent whenever fresh data arrives from the Tello and periodMs is ignored
// If asAvailable is false then updates are send every periodMs
// This streamer does not block on the channel, so unconsumed updates are lost
func (tello *Tello) StreamFlightData(asAvailable bool, periodMs time.Duration) (<-chan FlightData, error) {
	tello.fdMu.RLock()
	if tello.fdStreaming {
		tello.fdMu.RUnlock()
		return nil, errors.New("Already streaming data from this Tello")
	}
	tello.fdMu.RUnlock()
	fdChan := make(chan FlightData, 2)
	if asAvailable {
		log.Fatal("asAvailable FlightData stream not yet implemented") // TODO
	} else {
		go func() {
			for {
				tello.fdMu.RLock()
				select {
				case fdChan <- tello.fd:
				default:
				}
				tello.fdMu.RUnlock()
				time.Sleep(periodMs * time.Millisecond)
			}
		}()
	}
	tello.fdMu.Lock()
	tello.fdStreaming = true
	tello.fdMu.Unlock()

	return fdChan, nil
}

func (tello *Tello) controlResponseListener() {
	buff := make([]byte, 4096)

	for {
		n, err := tello.ctrlConn.Read(buff)

		// the initial connect response is different...
		if tello.ctrlConnecting && n == 11 {
			if bytes.ContainsAny(buff, "conn_ack:") {
				// TODO handle returned video port?
				log.Printf("Debug: conn_ack received, buffer len: %d\n", n)
				tello.ctrlMu.Lock()
				tello.ctrlConnecting = false
				tello.ctrlConnected = true
				tello.ctrlMu.Unlock()
			} else {
				log.Printf("Unexpected response to connection request <%s>\n", string(buff))
			}
			continue
		}

		select {
		case <-tello.ctrlStopChan:
			log.Println("ControlResponseLister stopped")
			return
		default:
		}
		if err != nil {
			log.Printf("Network Read Error - %v\n", err)
		} else {
			if buff[0] != msgHdr {
				log.Printf("Unexpected network message from Tello <%d>\n", buff[0])
			} else {
				pkt := bufferToPacket(buff)
				switch pkt.messageID {
				case msgFlightStatus:
				case msgLightStrength:
					// log.Printf("Light strength received - Size: %d, Type: %d\n", pkt.size13, pkt.packetType)
					tello.fdMu.Lock()
					tello.fd.LightStrength = uint8(pkt.payload[0])
					tello.fdMu.Unlock()
				case msgLogHeader:
					log.Printf("Log Header received - Size: %d, Type: %d\n", pkt.size13, pkt.packetType)
				case msgWifiStrength:
					// log.Printf("Wifi strength received - Size: %d, Type: %d\n", pkt.size13, pkt.packetType)
					tello.fdMu.Lock()
					tello.fd.WifiStrength = uint8(pkt.payload[0])
					tello.fd.WifiInterference = uint8(pkt.payload[1])
					log.Printf("Parsed Wifi Strength: %d, Interference: %d\n", tello.fd.WifiStrength, tello.fd.WifiInterference)
					tello.fdMu.Unlock()
				default:
					log.Printf("Unknown message from Tello - ID: <%d>, Size %d, Type: %d\n",
						pkt.messageID, pkt.size13, pkt.packetType)
				}
			}
		}

	}
}

func (tello *Tello) videoResponseListener() {

}

func (tello *Tello) sendConnectRequest(videoPort uint16) {
	// the initial connect request is different to the usual packets...
	msgBuff := []byte("conn_req:lh")
	msgBuff[9] = byte(videoPort & 0xff)
	msgBuff[10] = byte(videoPort >> 8)
	tello.ctrlMu.Lock()
	tello.ctrlConnecting = true
	tello.ctrlConn.Write(msgBuff)
	tello.ctrlMu.Unlock()
}

func (tello *Tello) keepAlive() {
	for {
		if tello.ctrlConnected {
			tello.sendStickUpdate()
		} else {
			return // we've disconnected
		}
		time.Sleep(keepAlivePeriodMs * time.Millisecond)
	}
}

func (tello *Tello) stickListener() {
	for {
		sm := <-tello.stickChan
		tello.ctrlMu.Lock()
		tello.ctrlLx = sm.Lx
		tello.ctrlLy = sm.Ly
		tello.ctrlRx = sm.Rx
		tello.ctrlRy = sm.Ry
		tello.ctrlThrottle = sm.Throttle
		tello.ctrlMu.Unlock()
	}
}
func jsFloatToTello(fv float64) uint64 {
	return uint64(660*fv + 1024)
}

func (tello *Tello) sendStickUpdate() {
	tello.ctrlMu.Lock()
	defer tello.ctrlMu.Unlock()
	// create the command packet
	var pkt packet

	// populate the command packet fields we need
	pkt.header = msgHdr
	pkt.toDrone = true
	pkt.packetType = ptData2
	pkt.messageID = msgSetStick
	pkt.sequence = 0
	pkt.payload = make([]byte, 11)

	// This packing of the joystick data is just vile...
	packedAxes := jsFloatToTello(tello.ctrlRx)
	packedAxes |= jsFloatToTello(tello.ctrlRy) << 11
	packedAxes |= jsFloatToTello(tello.ctrlLy) << 22
	packedAxes |= jsFloatToTello(tello.ctrlLx) << 33
	if tello.ctrlThrottle > 0.1 {
		packedAxes |= 0x7ff << 44
	} else {
		packedAxes |= jsFloatToTello(tello.ctrlThrottle) << 44
	}
	pkt.payload[0] = byte(packedAxes)
	pkt.payload[1] = byte(packedAxes >> 8)
	pkt.payload[2] = byte(packedAxes >> 16)
	pkt.payload[3] = byte(packedAxes >> 24)
	pkt.payload[4] = byte(packedAxes >> 32)
	pkt.payload[5] = byte(packedAxes >> 40)

	now := time.Now()
	pkt.payload[6] = byte(now.Hour())
	pkt.payload[7] = byte(now.Minute())
	pkt.payload[8] = byte(now.Second())
	ms := now.UnixNano() / 1000000
	pkt.payload[9] = byte(ms & 0xff)
	pkt.payload[10] = byte(ms >> 8)

	// pack the packet into raw format and calculate CRCs etc.
	buff := packetToBuffer(pkt)

	// send the command packet
	tello.ctrlConn.Write(buff)
}

// TakeOff sends a normal takeoff request to the Tello
func (tello *Tello) TakeOff() {
	tello.ctrlMu.Lock()
	defer tello.ctrlMu.Unlock()
	// create the command packet
	var pkt packet

	// populate the command packet fields we need
	pkt.header = msgHdr
	pkt.toDrone = true
	pkt.packetType = ptSet
	pkt.messageID = msgDoTakeoff
	tello.ctrlSeq++
	pkt.sequence = tello.ctrlSeq

	// pack the packet into raw format and calculate CRCs etc.
	buff := packetToBuffer(pkt)

	// send the command packet
	tello.ctrlConn.Write(buff)
}

// Land sends a normal Land request to the Tello
func (tello *Tello) Land() {
	tello.ctrlMu.Lock()
	defer tello.ctrlMu.Unlock()
	// create the command packet
	var pkt packet

	// populate the command packet fields we need
	pkt.header = msgHdr
	pkt.toDrone = true
	pkt.packetType = ptSet
	pkt.messageID = msgDoLand
	tello.ctrlSeq++
	pkt.sequence = tello.ctrlSeq
	pkt.payload = make([]byte, 1)
	pkt.payload[0] = 0

	// pack the packet into raw format and calculate CRCs etc.
	buff := packetToBuffer(pkt)

	// send the command packet
	tello.ctrlConn.Write(buff)
}

// Hover simply sets the sticks to zero - useful as a panic action!
func (tello *Tello) Hover() {
	tello.ctrlMu.Lock()
	tello.ctrlLx = 0
	tello.ctrlLy = 0
	tello.ctrlRx = 0
	tello.ctrlRy = 0
	tello.ctrlThrottle = 0
	tello.ctrlMu.Unlock()
}
