package rtsp

import (
	"bufio"
	"bytes"
	"crypto/md5"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bruce-qin/EasyGoLib/utils"

	"github.com/teris-io/shortid"
)

type RTPPack struct {
	Type   RTPType
	Buffer *bytes.Buffer
}

type SessionType int

const (
	SESSION_TYPE_PUSHER  SessionType = 1
	SESSEION_TYPE_PLAYER SessionType = 2
)

func (st SessionType) String() string {
	switch st {
	case SESSION_TYPE_PUSHER:
		return "pusher"
	case SESSEION_TYPE_PLAYER:
		return "player"
	}
	return "unknow"
}

type RTPType int

const (
	RTP_TYPE_AUDIO RTPType = iota
	RTP_TYPE_VIDEO
	RTP_TYPE_AUDIOCONTROL
	RTP_TYPE_VIDEOCONTROL
)

func (rt RTPType) String() string {
	switch rt {
	case RTP_TYPE_AUDIO:
		return "audio"
	case RTP_TYPE_VIDEO:
		return "video"
	case RTP_TYPE_AUDIOCONTROL:
		return "audio control"
	case RTP_TYPE_VIDEOCONTROL:
		return "video control"
	}
	return "unknow"
}

type TransType int

const (
	TRANS_TYPE_TCP TransType = iota
	TRANS_TYPE_UDP
)

func (tt TransType) String() string {
	switch tt {
	case TRANS_TYPE_TCP:
		return "TCP"
	case TRANS_TYPE_UDP:
		return "UDP"
	}
	return "unknow"
}

const UDP_BUF_SIZE = 1048576

type Session struct {
	SessionLogger
	ID        string
	Server    *Server
	Conn      *RichConn
	connRW    *bufio.ReadWriter
	connWLock sync.RWMutex
	Type      SessionType
	TransType TransType
	Path      string
	URL       string
	SDPRaw    string
	SDPMap    map[string]*SDPInfo

	localAuthorizationEnable      bool
	remoteHttpAuthorizationEnable bool
	authorizationType             AuthorizationType
	nonce                         string
	closeOld                      bool
	debugLogEnable                bool

	AControl string
	VControl string
	ACodec   string
	VCodec   string

	// stats info
	InBytes  int
	OutBytes int
	StartAt  time.Time
	Timeout  int

	Stoped bool

	//tcp channels
	aRTPChannel        int
	aRTPControlChannel int
	vRTPChannel        int
	vRTPControlChannel int
	//不为nil表示当前推流pusher
	multicastInfo       *MulticastCommunicateInfo
	multicastLastBoard  time.Time
	multicastBoardTimes int

	Pusher      *Pusher
	Player      *Player
	UDPClient   *UDPClient
	RTPHandles  []func(*RTPPack)
	StopHandles []func()

	rtpPackHandelChan chan *RTPPack
	requestHandelChan chan *Request
}

func (session *Session) String() string {
	return fmt.Sprintf("session[%v][%v][%s][%s][%s]", session.Type, session.TransType, session.Path, session.ID, session.Conn.RemoteAddr().String())
}

func NewSession(server *Server, conn net.Conn) *Session {
	networkBuffer := server.networkBuffer
	timeoutMillis := server.rtspTimeoutMillisecond
	timeoutTCPConn := &RichConn{conn, time.Duration(timeoutMillis) * time.Millisecond}
	session := &Session{
		ID:                            shortid.MustGenerate(),
		Server:                        server,
		Conn:                          timeoutTCPConn,
		connRW:                        bufio.NewReadWriter(bufio.NewReaderSize(timeoutTCPConn, networkBuffer), bufio.NewWriterSize(timeoutTCPConn, networkBuffer)),
		StartAt:                       time.Now(),
		Timeout:                       server.rtspTimeoutMillisecond,
		localAuthorizationEnable:      server.localAuthorizationEnable,
		remoteHttpAuthorizationEnable: server.remoteHttpAuthorizationEnable,
		authorizationType:             server.authorizationType,
		debugLogEnable:                server.debugLogEnable,
		RTPHandles:                    make([]func(*RTPPack), 0),
		StopHandles:                   make([]func(), 0),
		vRTPChannel:                   -1,
		vRTPControlChannel:            -1,
		aRTPChannel:                   -1,
		aRTPControlChannel:            -1,
		closeOld:                      server.closeOld,
		rtpPackHandelChan:             make(chan *RTPPack, 10),
		requestHandelChan:             make(chan *Request, 1),
	}

	session.logger = log.New(os.Stdout, fmt.Sprintf("[%s]", session.ID), log.LstdFlags|log.Lshortfile)
	if !utils.Debug {
		session.logger.SetOutput(utils.GetLogWriter())
	}

	return session
}

func (session *Session) Stop() {
	if session.Stoped {
		return
	}
	session.Stoped = true
	go session.ToCloseWebHookInfo().ExecuteWebHookNotify()
	for _, h := range session.StopHandles {
		h()
	}
	if session.Conn != nil {
		session.connRW.Flush()
		session.Conn.Close()
		session.Conn = nil
	}
	if session.UDPClient != nil {
		session.UDPClient.Stop()
		session.UDPClient = nil
	}
}

func (session *Session) startRtpHandler() {
	for !session.Stoped {
		if pack, ok := <-session.rtpPackHandelChan; ok {
			for _, h := range session.RTPHandles {
				h(pack)
			}
		}
	}
}

func (session *Session) startRequestHandler() {
	for !session.Stoped {
		if req, ok := <-session.requestHandelChan; ok {
			session.handleRequest(req)
		}
	}
}

func (session *Session) Start() {
	defer session.Stop()
	buf1 := make([]byte, 1)
	buf2 := make([]byte, 2)
	logger := session.logger
	timer := time.Unix(0, 0)
	go session.startRtpHandler()
	go session.startRequestHandler()
	for !session.Stoped {
		if _, err := io.ReadFull(session.connRW, buf1); err != nil {
			if session.Path != "" {
				logger.Println("stop ", session.Type, ":", session, "; path: ", session.Path, "; error info:", err)
			}
			return
		}
		if buf1[0] == 0x24 { //rtp data
			if _, err := io.ReadFull(session.connRW, buf1); err != nil {
				logger.Println("stop ", session.Type, ":", session, "; path: ", session.Path, "; error info:", err)
				return
			}
			if _, err := io.ReadFull(session.connRW, buf2); err != nil {
				logger.Println("stop ", session.Type, ":", session, "; path: ", session.Path, "; error info:", err)
				return
			}
			channel := int(buf1[0])
			rtpLen := int(binary.BigEndian.Uint16(buf2))
			rtpBytes := make([]byte, rtpLen)
			if _, err := io.ReadFull(session.connRW, rtpBytes); err != nil {
				logger.Println("stop ", session.Type, ":", session, "; path: ", session.Path, "; error info:", err)
				return
			}
			rtpBuf := bytes.NewBuffer(rtpBytes)
			var pack *RTPPack
			switch channel {
			case session.aRTPChannel:
				pack = &RTPPack{
					Type:   RTP_TYPE_AUDIO,
					Buffer: rtpBuf,
				}
				elapsed := time.Now().Sub(timer)
				if elapsed >= 30*time.Second {
					logger.Println("Recv an audio RTP package")
					timer = time.Now()
				}
			case session.aRTPControlChannel:
				pack = &RTPPack{
					Type:   RTP_TYPE_AUDIOCONTROL,
					Buffer: rtpBuf,
				}
			case session.vRTPChannel:
				pack = &RTPPack{
					Type:   RTP_TYPE_VIDEO,
					Buffer: rtpBuf,
				}
				elapsed := time.Now().Sub(timer)
				if elapsed >= 30*time.Second {
					logger.Println("Recv an video RTP package")
					timer = time.Now()
				}
			case session.vRTPControlChannel:
				pack = &RTPPack{
					Type:   RTP_TYPE_VIDEOCONTROL,
					Buffer: rtpBuf,
				}
			default:
				logger.Printf("unknow rtp pack type, %v", channel)
				continue
			}
			session.InBytes += rtpLen + 4
			session.rtpPackHandelChan <- pack
			//for _, h := range session.RTPHandles {
			//	h(pack)
			//}
		} else { // rtsp cmd
			reqBuf := bytes.NewBuffer(nil)
			reqBuf.Write(buf1)
			for !session.Stoped {
				if line, isPrefix, err := session.connRW.ReadLine(); err != nil {
					if session.Path != "" {
						logger.Println("rtsp protocol transform error, stop ", session.Type, ":", session, "; path: ", session.Path, "; error info:", err)
					}
					return
				} else {
					reqBuf.Write(line)
					if !isPrefix {
						reqBuf.WriteString("\r\n")
					}
					if len(line) == 0 {
						req := NewRequest(reqBuf.String())
						if req == nil {
							break
						}
						session.InBytes += reqBuf.Len()
						contentLen := req.GetContentLength()
						session.InBytes += contentLen
						if contentLen > 0 {
							bodyBuf := make([]byte, contentLen)
							if n, err := io.ReadFull(session.connRW, bodyBuf); err != nil {
								logger.Println("rtsp protocol transform error:", err)
								return
							} else if n != contentLen {
								logger.Printf("rtsp protocol transform error, read rtsp request body failed, expect size[%d], got size[%d]", contentLen, n)
								return
							}
							req.Body = string(bodyBuf)
						}
						session.requestHandelChan <- req
						//session.handleRequest(req)
						break
					}
				}
			}
		}
	}
}

func (session *Session) handleRequest(req *Request) {
	//if session.Timeout > 0 {
	//	session.Conn.SetDeadline(time.Now().Add(time.Duration(session.Timeout) * time.Second))
	//}
	logger := session.logger
	logger.Printf("<<<\n%s", req)
	res := NewResponse(200, "OK", req.Header["CSeq"], session.ID, "")
	defer func() {
		if p := recover(); p != nil {
			logger.Printf("handleRequest err ocurs:%v", p)
			res.StatusCode = 500
			res.Status = fmt.Sprintf("Inner Server Error, %v", p)
		}
		logger.Printf(">>>\n%s", res)
		outBytes := []byte(res.String())
		session.connWLock.Lock()
		session.connRW.Write(outBytes)
		session.connRW.Flush()
		session.connWLock.Unlock()
		session.OutBytes += len(outBytes)
		switch req.Method {
		case "PLAY", "RECORD":
			//开始拉流，开始推流
			switch session.Type {
			case SESSEION_TYPE_PLAYER:
				if session.Pusher.HasPlayer(session.Player) {
					session.Player.Pause(false)
				} else {
					session.Pusher.AddPlayer(session.Player)
				}
				// case SESSION_TYPE_PUSHER:
				// 	session.Server.AddPusher(session.Pusher)
			}
		case "TEARDOWN":
			{
				//停止推流
				session.Stop()
				return
			}
		}
		if res.StatusCode != 200 && res.StatusCode != 401 {
			logger.Printf("Response request error[%d]. stop session.", res.StatusCode)
			session.Stop()
		}
	}()
	if req.Method != "OPTIONS" {
		if session.localAuthorizationEnable || session.remoteHttpAuthorizationEnable {
			authLine := req.Header["Authorization"]
			authFailed := true
			if authLine != "" {
				sessionType := session.Type
				if sessionType == 0 {
					if req.Method == "ANNOUNCE" {
						sessionType = SESSION_TYPE_PUSHER
					} else if req.Method == "DESCRIBE" {
						sessionType = SESSEION_TYPE_PLAYER
					} else {
						res.StatusCode = 403
						res.Status = "Method Error"
						return
					}
				}
				info, err := DecodeAuthorizationInfo(authLine, session.nonce, req.Method, sessionType)
				if err != nil {
					logger.Printf("%v", err)
				} else if session.localAuthorizationEnable {
					if err = info.CheckAuthLocal(); err != nil {
						logger.Printf("%v", err)
					} else {
						authFailed = false
					}
				} else if session.remoteHttpAuthorizationEnable {
					if err = info.CheckAuthHttpRemote(); err != nil {
						logger.Printf("%v", err)
					} else {
						authFailed = false
					}
				}
			}
			if authFailed {
				res.StatusCode = 401
				res.Status = "Unauthorized"
				if session.Server.authorizationType == "Basic" {
					res.Header["WWW-Authenticate"] = `Basic realm="EasyDarwin"`
				} else {
					nonce := fmt.Sprintf("%x", md5.Sum([]byte(shortid.MustGenerate())))
					session.nonce = nonce
					res.Header["WWW-Authenticate"] = fmt.Sprintf(`Digest realm="EasyDarwin", nonce="%s", algorithm="MD5"`, nonce)
				}
				return
			}
		}
	}
	switch req.Method {
	case "OPTIONS":
		res.Header["Public"] = "DESCRIBE, SETUP, TEARDOWN, PLAY, PAUSE, OPTIONS, ANNOUNCE, RECORD"
	case "ANNOUNCE":
		//推流
		session.Type = SESSION_TYPE_PUSHER
		session.URL = req.URL

		surl, err := url.Parse(req.URL)
		if err != nil {
			res.StatusCode = 500
			res.Status = "Invalid URL"
			return
		}
		if continueProcess := NewWebHookInfo(ON_PUBLISH, session.ID, SESSION_TYPE_PUSHER, TRANS_TYPE_TCP, req.URL, surl.Path, req.Body, session.Conn.RemoteAddr().String()).ExecuteWebHookNotify(); !continueProcess {
			res.StatusCode = 500
			res.Status = "Server not allowed you push stream"
			return
		}

		session.Path = surl.Path

		session.SDPRaw = req.Body
		session.SDPMap = ParseSDP(req.Body)
		sdp, ok := session.SDPMap["audio"]
		if ok {
			session.AControl = sdp.Control
			session.ACodec = sdp.Codec
			logger.Printf("audio codec[%s]\n", session.ACodec)
		}
		sdp, ok = session.SDPMap["video"]
		if ok {
			session.VControl = sdp.Control
			session.VCodec = sdp.Codec
			logger.Printf("video codec[%s]\n", session.VCodec)
		}
		addPusher := false
		if session.closeOld {
			r, _ := session.Server.TryAttachToPusher(session)
			if r < -1 {
				logger.Printf("reject pusher.")
				res.StatusCode = 406
				res.Status = "Not Acceptable"
			} else if r == 0 {
				addPusher = true
			} else {
				logger.Printf("Attached to old pusher")
				// 尝试发给客户端ANNOUCE
				// players := pusher.GetPlayers()
				// for _, v := range players {
				// 	sess := v.Session

				// 	hearers := make(map[string]string)
				// 	hearers["Content-Type"] = "application/sdp"
				// 	hearers["Session"] = sess.ID
				// 	hearers["Content-Length"] = strconv.Itoa(len(v.SDPRaw))
				// 	var req = Request{Method: ANNOUNCE, URL: v.URL, Version: "1.0", Header: hearers, Body: pusher.SDPRaw()}
				// 	sess.connWLock.Lock()
				// 	logger.Println(req.String())
				// 	outBytes := []byte(req.String())
				// 	sess.connRW.Write(outBytes)
				// 	sess.connRW.Flush()
				// 	sess.connWLock.Unlock()
				// }
			}
		} else {
			addPusher = true
		}
		if addPusher {
			session.Pusher = NewPusher(session)
			addedToServer := session.Server.AddPusher(session.Pusher)
			if !addedToServer {
				logger.Printf("reject pusher.")
				res.StatusCode = 406
				res.Status = "Not Acceptable"
			}
		}
	case "DESCRIBE":
		//拉流
		session.Type = SESSEION_TYPE_PLAYER
		session.URL = req.URL

		url, err := url.Parse(req.URL)
		if err != nil {
			res.StatusCode = 500
			res.Status = "Invalid URL"
			return
		}
		if continueProcess := NewWebHookInfo(ON_PLAY, session.ID, SESSEION_TYPE_PLAYER, TRANS_TYPE_TCP, req.URL, url.Path, req.Body, session.Conn.RemoteAddr().String()).ExecuteWebHookNotify(); !continueProcess {
			res.StatusCode = 500
			res.Status = "Server not allowed you pull stream"
			return
		}
		session.Path = url.Path
		pusher := session.Server.pushers[session.Path]
		if pusher == nil {
			waitExist := false
			if session.Server.streamNotExistHoldMillisecond != 0 {
				end := time.Now().Add(session.Server.streamNotExistHoldMillisecond)
				for time.Now().Before(end) {
					pusher = session.Server.pushers[session.Path]
					if pusher == nil {
						time.Sleep(time.Duration(200) * time.Millisecond)
					} else {
						waitExist = true
						break
					}
				}
			}
			if !waitExist {
				res.StatusCode = 404
				res.Status = "NOT FOUND"
				return
			}
		}
		session.Player = NewPlayer(session, pusher)
		session.Pusher = pusher
		session.AControl = pusher.AControl()
		session.VControl = pusher.VControl()
		session.ACodec = pusher.ACodec()
		session.VCodec = pusher.VCodec()
		session.Conn.timeout = 0
		session.logger = log.New(os.Stdout, fmt.Sprintf("[player:%s, pusher:%s, path: %s]", session.ID, pusher.ID(), session.Path), log.LstdFlags|log.Lshortfile)
		res.SetBody(session.Pusher.SDPRaw())
	case "SETUP":
		ts := req.Header["Transport"]
		// control字段可能是`stream=1`字样，也可能是rtsp://...字样。即control可能是url的path，也可能是整个url
		// 例1：
		// a=control:streamid=1
		// 例2：
		// a=control:rtsp://192.168.1.64/trackID=1
		// 例3：
		// a=control:?ctype=video
		setupUrl, err := url.Parse(req.URL)
		if err != nil {
			res.StatusCode = 500
			res.Status = "Invalid URL"
			return
		}
		if setupUrl.Port() == "" {
			setupUrl.Host = fmt.Sprintf("%s:554", setupUrl.Host)
		}
		setupPath := setupUrl.String()

		// error status. SETUP without ANNOUNCE or DESCRIBE.
		if session.Pusher == nil {
			res.StatusCode = 500
			res.Status = "Error Status"
			return
		}
		//setupPath = setupPath[strings.LastIndex(setupPath, "/")+1:]
		vPath := ""
		if strings.Index(strings.ToLower(session.VControl), "rtsp://") == 0 {
			vControlUrl, err := url.Parse(session.VControl)
			if err != nil {
				res.StatusCode = 500
				res.Status = "Invalid VControl"
				return
			}
			if vControlUrl.Port() == "" {
				vControlUrl.Host = fmt.Sprintf("%s:554", vControlUrl.Host)
			}
			vPath = vControlUrl.String()
		} else {
			vPath = session.VControl
		}

		aPath := ""
		if strings.Index(strings.ToLower(session.AControl), "rtsp://") == 0 {
			aControlUrl, err := url.Parse(session.AControl)
			if err != nil {
				res.StatusCode = 500
				res.Status = "Invalid AControl"
				return
			}
			if aControlUrl.Port() == "" {
				aControlUrl.Host = fmt.Sprintf("%s:554", aControlUrl.Host)
			}
			aPath = aControlUrl.String()
		} else {
			aPath = session.AControl
		}

		mtcp := regexp.MustCompile("interleaved=(\\d+)(-(\\d+))?")
		mudp := regexp.MustCompile("client_port=(\\d+)(-(\\d+))?")

		if tcpMatchs := mtcp.FindStringSubmatch(ts); tcpMatchs != nil {
			session.TransType = TRANS_TYPE_TCP
			if setupPath == aPath || aPath != "" && strings.LastIndex(setupPath, aPath) == len(setupPath)-len(aPath) {
				session.aRTPChannel, _ = strconv.Atoi(tcpMatchs[1])
				session.aRTPControlChannel, _ = strconv.Atoi(tcpMatchs[3])
			} else if setupPath == vPath || vPath != "" && strings.LastIndex(setupPath, vPath) == len(setupPath)-len(vPath) {
				session.vRTPChannel, _ = strconv.Atoi(tcpMatchs[1])
				session.vRTPControlChannel, _ = strconv.Atoi(tcpMatchs[3])
			} else {
				res.StatusCode = 500
				res.Status = fmt.Sprintf("SETUP [TCP] got UnKown control:%s", setupPath)
				logger.Printf("SETUP [TCP] got UnKown control:%s", setupPath)
			}
			logger.Printf("Parse SETUP req.TRANSPORT:TCP.Session.Type:%d,control:%s, AControl:%s,VControl:%s", session.Type, setupPath, aPath, vPath)
		} else if udpMatchs := mudp.FindStringSubmatch(ts); udpMatchs != nil {
			session.TransType = TRANS_TYPE_UDP
			// no need for tcp timeout.
			session.Conn.timeout = 0
			if session.Type == SESSEION_TYPE_PLAYER && session.UDPClient == nil {
				session.UDPClient = &UDPClient{
					Session: session,
				}
			}
			if session.Type == SESSION_TYPE_PUSHER && session.Pusher.UDPServer == nil {
				session.Pusher.UDPServer = &UDPServer{
					Session: session,
				}
			}
			logger.Printf("Parse SETUP req.TRANSPORT:UDP.Session.Type:%d,control:%s, AControl:%s,VControl:%s", session.Type, setupPath, aPath, vPath)
			if setupPath == aPath || aPath != "" && strings.LastIndex(setupPath, aPath) == len(setupPath)-len(aPath) {
				if session.Type == SESSEION_TYPE_PLAYER {
					session.UDPClient.APort, _ = strconv.Atoi(udpMatchs[1])
					session.UDPClient.AControlPort, _ = strconv.Atoi(udpMatchs[3])
					if err := session.UDPClient.SetupAudio(); err != nil {
						res.StatusCode = 500
						res.Status = fmt.Sprintf("udp client setup audio error, %v", err)
						return
					}
				}
				if session.Type == SESSION_TYPE_PUSHER {
					if err := session.Pusher.UDPServer.SetupAudio(); err != nil {
						res.StatusCode = 500
						res.Status = fmt.Sprintf("udp server setup audio error, %v", err)
						return
					}
					tss := strings.Split(ts, ";")
					idx := -1
					for i, val := range tss {
						if val == udpMatchs[0] {
							idx = i
						}
					}
					tail := append([]string{}, tss[idx+1:]...)
					tss = append(tss[:idx+1], fmt.Sprintf("server_port=%d-%d", session.Pusher.UDPServer.APort, session.Pusher.UDPServer.AControlPort))
					tss = append(tss, tail...)
					ts = strings.Join(tss, ";")
				}
			} else if setupPath == vPath || vPath != "" && strings.LastIndex(setupPath, vPath) == len(setupPath)-len(vPath) {
				if session.Type == SESSEION_TYPE_PLAYER {
					session.UDPClient.VPort, _ = strconv.Atoi(udpMatchs[1])
					session.UDPClient.VControlPort, _ = strconv.Atoi(udpMatchs[3])
					if err := session.UDPClient.SetupVideo(); err != nil {
						res.StatusCode = 500
						res.Status = fmt.Sprintf("udp client setup video error, %v", err)
						return
					}
				}

				if session.Type == SESSION_TYPE_PUSHER {
					if err := session.Pusher.UDPServer.SetupVideo(); err != nil {
						res.StatusCode = 500
						res.Status = fmt.Sprintf("udp server setup video error, %v", err)
						return
					}
					tss := strings.Split(ts, ";")
					idx := -1
					for i, val := range tss {
						if val == udpMatchs[0] {
							idx = i
						}
					}
					tail := append([]string{}, tss[idx+1:]...)
					tss = append(tss[:idx+1], fmt.Sprintf("server_port=%d-%d", session.Pusher.UDPServer.VPort, session.Pusher.UDPServer.VControlPort))
					tss = append(tss, tail...)
					ts = strings.Join(tss, ";")
				}
			} else {
				logger.Printf("SETUP [UDP] got UnKown control:%s", setupPath)
			}
		}
		res.Header["Transport"] = ts
	case "PLAY":
		//开始拉流
		// error status. PLAY without ANNOUNCE or DESCRIBE.
		if session.Pusher == nil {
			res.StatusCode = 500
			res.Status = "Error Status"
			return
		}
		res.Header["Range"] = req.Header["Range"]
	case "RECORD":
		//开始推流
		// error status. RECORD without ANNOUNCE or DESCRIBE.
		if session.Pusher == nil {
			res.StatusCode = 500
			res.Status = "Error Status"
			return
		}
	case "PAUSE":
		if session.Player == nil {
			res.StatusCode = 500
			res.Status = "Error Status"
			return
		}
		session.Player.Pause(true)
	}
}

func (session *Session) SendRTP(pack *RTPPack) (err error) {
	if pack == nil {
		err = fmt.Errorf("player send rtp got nil pack")
		return
	}
	if session.TransType == TRANS_TYPE_UDP {
		if session.UDPClient == nil {
			err = fmt.Errorf("player use udp transport but udp client not found")
			return
		}
		err = session.UDPClient.SendRTP(pack)
		return
	}
	switch pack.Type {
	case RTP_TYPE_AUDIO:
		bufChannel := make([]byte, 2)
		bufChannel[0] = 0x24
		bufChannel[1] = byte(session.aRTPChannel)
		session.connWLock.Lock()
		session.connRW.Write(bufChannel)
		bufLen := make([]byte, 2)
		binary.BigEndian.PutUint16(bufLen, uint16(pack.Buffer.Len()))
		session.connRW.Write(bufLen)
		session.connRW.Write(pack.Buffer.Bytes())
		session.connRW.Flush()
		session.connWLock.Unlock()
		session.OutBytes += pack.Buffer.Len() + 4
	case RTP_TYPE_AUDIOCONTROL:
		bufChannel := make([]byte, 2)
		bufChannel[0] = 0x24
		bufChannel[1] = byte(session.aRTPControlChannel)
		session.connWLock.Lock()
		session.connRW.Write(bufChannel)
		bufLen := make([]byte, 2)
		binary.BigEndian.PutUint16(bufLen, uint16(pack.Buffer.Len()))
		session.connRW.Write(bufLen)
		session.connRW.Write(pack.Buffer.Bytes())
		session.connRW.Flush()
		session.connWLock.Unlock()
		session.OutBytes += pack.Buffer.Len() + 4
	case RTP_TYPE_VIDEO:
		bufChannel := make([]byte, 2)
		bufChannel[0] = 0x24
		bufChannel[1] = byte(session.vRTPChannel)
		session.connWLock.Lock()
		session.connRW.Write(bufChannel)
		bufLen := make([]byte, 2)
		binary.BigEndian.PutUint16(bufLen, uint16(pack.Buffer.Len()))
		session.connRW.Write(bufLen)
		session.connRW.Write(pack.Buffer.Bytes())
		session.connRW.Flush()
		session.connWLock.Unlock()
		session.OutBytes += pack.Buffer.Len() + 4
	case RTP_TYPE_VIDEOCONTROL:
		bufChannel := make([]byte, 2)
		bufChannel[0] = 0x24
		bufChannel[1] = byte(session.vRTPControlChannel)
		session.connWLock.Lock()
		session.connRW.Write(bufChannel)
		bufLen := make([]byte, 2)
		binary.BigEndian.PutUint16(bufLen, uint16(pack.Buffer.Len()))
		session.connRW.Write(bufLen)
		session.connRW.Write(pack.Buffer.Bytes())
		session.connRW.Flush()
		session.connWLock.Unlock()
		session.OutBytes += pack.Buffer.Len() + 4
	default:
		err = fmt.Errorf("session tcp send rtp got unkown pack type[%v]", pack.Type)
	}
	return
}
