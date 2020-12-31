package main

import (
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/caddyserver/certmagic"
	"github.com/libdns/duckdns"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"
)

const (
	rtcpPLIInterval = time.Second * 3
)

var peerConnectionConfig = webrtc.Configuration{
	ICEServers: []webrtc.ICEServer{
		{
			URLs: []string{"stun:stun.l.google.com:19302"},
		},
	},
}

var (
	pubStartCount uint32
	localTrack    *webrtc.TrackLocalStaticRTP
	subMap        map[string]*webrtc.PeerConnection = make(map[string]*webrtc.PeerConnection)
	subMapMutex   sync.Mutex
)

func checkPanic(err error) {
	if err != nil {
		panic(err)
	}
}

func slashHandler(res http.ResponseWriter, req *http.Request) {
	res.Header().Set("Content-Type", "text/html")

	if req.URL.Path != "/" {
		http.Error(res, "404 - page not found", http.StatusNotFound)
		return
	}

	buf, err := ioutil.ReadFile("html/index.html")
	if err != nil {
		http.Error(res, "can't open index.html", http.StatusInternalServerError)
		return
	}
	_, _ = res.Write(buf)
}

//var silenceJanus = flag.Bool("silence-janus", false, "if true will throw away janus output")
var debug = flag.Bool("debug", true, "enable debug output")
var nohtml = flag.Bool("no-html", false, "do not serve any html files, only do WHIP")

var info = log.New(os.Stderr, "I ", log.Lmicroseconds|log.LUTC)
var elog = log.New(os.Stderr, "E ", log.Lmicroseconds|log.LUTC)

func main() {
	var err error
	flag.Parse()

	if *debug {
		log.SetFlags(log.Lmicroseconds | log.LUTC)
		log.SetPrefix("D ")
		info.Println("debug output IS enabled")
	} else {
		info.Println("debug output NOT enabled")
		log.SetOutput(ioutil.Discard)
		log.SetPrefix("")
		log.SetFlags(0)
	}

	//unclear if we need
	//var group *errgroup.Group

	// ln, err := net.Listen("tcp", ":8000")
	// checkPanic(err)

	mux := http.NewServeMux()

	pubPath := "/pub"
	subPath := "/sub/" // 2nd slash important

	if !*nohtml {
		mux.HandleFunc("/", slashHandler)
	}
	mux.HandleFunc(pubPath, pubHandler)
	mux.HandleFunc(subPath, subHandler)

	

	ducktoken := os.Getenv("DUCKDNS")


	//certmagic.DefaultACME.Email = ""

	//I find this function from certmagic more opaque than I like
	//err = HTTPS([]string{"x186k.duckdns.org"}, mux, ducktoken) // https automagic
	//panic(err)

	certmagic.DefaultACME.Agreed = true
	
	certmagic.DefaultACME.DNS01Solver = &certmagic.DNS01Solver{
		DNSProvider:        &duckdns.Provider{APIToken: ducktoken},
		TTL:                0,
		PropagationTimeout: 0,
		Resolvers:          []string{},
	}


	// We do NOT do port 80 redirection, as 
	tlsConfig, err := certmagic.TLS([]string{"x186k.duckdns.org"})
	checkPanic(err)
	/// XXX ro work with OBS studio for now
	tlsConfig.MinVersion = 0  



	ln,err:=tls.Listen("tcp", ":8000", tlsConfig)
	checkPanic(err)

	log.Println("WHIP input listener at:", ln.Addr().String(), pubPath)
	log.Println("WHIP output listener at:", ln.Addr().String(), subPath)

	err = http.Serve(ln, mux)
	panic(err)

}

func mstime() string {
	const timeformatutc = "2006-01-02T15:04:05.000Z07:00"
	return time.Now().UTC().Format(timeformatutc)
}

// sends error to stderr and http.ResponseWriter with time
func teeErrorStderrHttp(w http.ResponseWriter, err error) {
	m := mstime() + " :: " + err.Error()
	elog.Println(m)
	http.Error(w, m, http.StatusInternalServerError)
}

// sfu ingest setup
func pubHandler(w http.ResponseWriter, req *http.Request) {
	defer req.Body.Close()

	log.Println("pubHandler request:", req.URL.String())

	n := atomic.AddUint32(&pubStartCount, 1)
	if n > 1 {
		teeErrorStderrHttp(w, errors.New("cannot accept 2nd ingress connection, restart for new session"))
		return
	}

	raw, err := ioutil.ReadAll(req.Body)
	if err != nil {
		teeErrorStderrHttp(w, err)
		return
	}

	// Create a new RTCPeerConnection
	peerConnection, err := webrtc.NewPeerConnection(peerConnectionConfig)
	if err != nil {
		panic(err)
	}

	// Allow us to receive 1 video track
	if _, err = peerConnection.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo); err != nil {
		panic(err)
	}

	// Set a handler for when a new remote track starts, this just distributes all our packets
	// to connected peers
	peerConnection.OnTrack(func(remoteTrack *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		// Send a PLI on an interval so that the publisher is pushing a keyframe every rtcpPLIInterval
		// This can be less wasteful by processing incoming RTCP events, then we would emit a NACK/PLI when a viewer requests it
		go func() {
			ticker := time.NewTicker(rtcpPLIInterval)
			for range ticker.C {
				if rtcpSendErr := peerConnection.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: uint32(remoteTrack.SSRC())}}); rtcpSendErr != nil {
					fmt.Println(rtcpSendErr)
				}
			}
		}()

		// Create a local track, all our SFU clients will be fed via this track
		localTrack, err = webrtc.NewTrackLocalStaticRTP(remoteTrack.Codec().RTPCodecCapability, "video", "pion")
		if err != nil {
			panic(err)
		}

		rtpBuf := make([]byte, 1400)
		for {
			i, _, readErr := remoteTrack.Read(rtpBuf)
			if readErr != nil {
				panic(readErr)
			}

			// ErrClosedPipe means we don't have any subscribers, this is ok if no peers have connected yet
			if _, err = localTrack.Write(rtpBuf[:i]); err != nil && !errors.Is(err, io.ErrClosedPipe) {
				panic(err)
			}
		}
	})

	// Set the remote SessionDescription
	desc := webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: string(raw)}
	err = peerConnection.SetRemoteDescription(desc)
	if err != nil {
		panic(err)
	}

	// Create answer
	answer, err := peerConnection.CreateAnswer(nil)
	if err != nil {
		panic(err)
	}

	// Create channel that is blocked until ICE Gathering is complete
	gatherComplete := webrtc.GatheringCompletePromise(peerConnection)

	// Sets the LocalDescription, and starts our UDP listeners
	err = peerConnection.SetLocalDescription(answer)
	if err != nil {
		panic(err)
	}

	// Block until ICE Gathering is complete, disabling trickle ICE
	// we do this because we only can exchange one signaling message
	// in a production application you should exchange ICE Candidates via OnICECandidate
	<-gatherComplete

	// Get the LocalDescription and take it to base64 so we can paste in browser
	answerDesc := *peerConnection.LocalDescription()

	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte(answerDesc.SDP))
	//Do NOT use http.error to return SDPs
	//http.Error(w, answer.SDP, http.StatusAccepted) //202 https://tools.ietf.org/html/draft-murillo-whip-00
}

// sfu egress setup
func subHandler(w http.ResponseWriter, httpreq *http.Request) {
	defer httpreq.Body.Close()

	log.Println("subHandler request", httpreq.URL.String())

	if localTrack == nil {
		teeErrorStderrHttp(w, fmt.Errorf("no publisher"))
		return
	}

	lastelement := path.Base(httpreq.URL.Path)
	if !strings.HasPrefix(lastelement, "txid=") || len(lastelement) < 15 {
		//can handle, do not panic
		teeErrorStderrHttp(w, fmt.Errorf("last element of sfuout url must start with txid= and be 15 chars or more"))
		return
	}
	txid := lastelement[5:]

	// empty or answer
	raw, err := ioutil.ReadAll(httpreq.Body)
	if err != nil {
		teeErrorStderrHttp(w, err)
		return
	}
	emptyOrAnswer := string(raw)

	if len(emptyOrAnswer) == 0 { // empty
		// part one of two part transaction

		// Create a new PeerConnection
		peerConnection, err := webrtc.NewPeerConnection(peerConnectionConfig)
		if err != nil {
			panic(err)
		}

		// AddTrack should be called before CreateOffer
		rtpSender, err := peerConnection.AddTrack(localTrack)
		if err != nil {
			panic(err)
		}

		// Read incoming RTCP packets
		// Before these packets are retuned they are processed by interceptors. For things
		// like NACK this needs to be called.
		go func() {
			rtcpBuf := make([]byte, 1500)
			for {
				if _, _, rtcpErr := rtpSender.Read(rtcpBuf); rtcpErr != nil {
					return
				}
			}
		}()

		// Create an offer for the other PeerConnection
		offer, err := peerConnection.CreateOffer(nil)
		if err != nil {
			panic(err)
		}

		// Create channel that is blocked until ICE Gathering is complete
		gatherComplete := webrtc.GatheringCompletePromise(peerConnection)

		// SetLocalDescription, needed before remote gets offer
		if err = peerConnection.SetLocalDescription(offer); err != nil {
			panic(err)
		}

		// Block until ICE Gathering is complete, disabling trickle ICE
		// we do this because we only can exchange one signaling message
		// in a production application you should exchange ICE Candidates via OnICECandidate
		<-gatherComplete

		subMapMutex.Lock()
		subMap[txid] = peerConnection
		subMapMutex.Unlock()
		// delete the map entry in one minute. should be plenty of time
		go func() {
			time.Sleep(time.Minute)
			subMapMutex.Lock()
			delete(subMap, txid)
			subMapMutex.Unlock()
		}()

		o := *peerConnection.LocalDescription()

		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(o.SDP))

		return
	} else {
		// part two of two part transaction
		log.Println("sub: 2nd request with answer")
		log.Println("sub: whip provided sdp starts with v=0", strings.HasPrefix(emptyOrAnswer, "v="))

		subMapMutex.Lock()
		peerConnection := subMap[txid]
		subMapMutex.Unlock()

		log.Println(emptyOrAnswer)
		sdesc := webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: emptyOrAnswer}

		err = peerConnection.SetRemoteDescription(sdesc)
		checkPanic(err)

		subMapMutex.Lock()
		delete(subMap, txid) //no error if missing
		subMapMutex.Unlock()

		log.Println("setremote done")
	}
}
