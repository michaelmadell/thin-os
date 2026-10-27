package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

//go:embed ui/*
var uiAssets embed.FS

type KioskMode struct {
	AllowEphemeral          bool `json:"allow_ephemeral_connections"`
	AllowLocalUSBConfig     bool `json:"allow_local_usb_config"`
	AllowLocalDisplayConfig bool `json:"allow_local_display_audio_config"`
	AutoLaunchDefault       bool `json:"auto_launch_default_connection"`
}

type Connection struct {
	ID                   string `json:"id"`
	Name                 string `json:"name"`
	App                  string `json:"app"`
	ServerURL            string `json:"server_url"`
	Domain               string `json:"domain"`
	Username             string `json:"username"`
	Password             string `json:"password,omitempty"`
	AllMonitors          bool   `json:"all_monitors"`
	SmartCardPassthrough bool   `json:"smartcard_passthrough"`
}

type USBRule struct {
	VID         string `json:"vid"`
	PID         string `json:"pid"`
	Description string `json:"description"`
	Mode        string `json:"mode"`
}

type Policy struct {
	ProfileName         string       `json:"profile_name"`
	ServerURL           string       `json:"management_server_url"`
	KioskMode           KioskMode    `json:"kiosk_mode"`
	EnabledApps         []string     `json:"enabled_apps"`
	DefaultConnectionID string       `json:"default_connection_id"`
	Connections         []Connection `json:"connections"`
	USBRules            []USBRule    `json:"usb_rules"`
}

var (
	policyMu      sync.RWMutex
	currentPolicy Policy
	
	mgmtURLMu     sync.RWMutex
	mgmtServerURL = "http://10.0.2.2:9090"
	
	lastLaunchMsg = "Ready"
	lastLaunchMu  sync.RWMutex
)

func setLaunchStatus(msg string) {
	lastLaunchMu.Lock()
	lastLaunchMsg = msg
	lastLaunchMu.Unlock()
	log.Println(msg)
}

func loadDefaultPolicy() {
	if envURL := os.Getenv("MGMT_SERVER_URL"); envURL != "" {
		mgmtServerURL = envURL
	}
	currentPolicy = Policy{
		ProfileName: "Standalone Mode (Server Unreachable)",
		ServerURL:   mgmtServerURL,
		KioskMode: KioskMode{
			AllowEphemeral: true, AllowLocalUSBConfig: true, AllowLocalDisplayConfig: true, AutoLaunchDefault: false,
		},
		EnabledApps: []string{"horizon", "citrix", "dcv", "rdp"},
		Connections: []Connection{},
		USBRules: []USBRule{},
	}
}

func syncWithManagementServer() {
	hostname, _ := os.Hostname()
	client := &http.Client{Timeout: 3 * time.Second}
	for {
		mgmtURLMu.RLock()
		url := mgmtServerURL
		mgmtURLMu.RUnlock()

		resp, err := client.Get(fmt.Sprintf("%s/api/v1/client/policy?hostname=%s", url, hostname))
		if err == nil && resp.StatusCode == 200 {
			var newPol Policy
			if json.NewDecoder(resp.Body).Decode(&newPol) == nil {
				policyMu.Lock()
				currentPolicy = newPol
				policyMu.Unlock()
			}
			resp.Body.Close()
		}
		time.Sleep(5 * time.Second)
	}
}

func launchVDIProcess(conn Connection) error {
	var cmd *exec.Cmd

	switch conn.App {
	case "horizon":
		bin := "/usr/bin/horizon-client"
		if _, err := os.Stat(bin); err != nil { bin = "/usr/bin/vmware-view" }
		args := []string{"--serverURL=" + conn.ServerURL}
		if conn.AllMonitors { args = append(args, "--allmonitors") }
		if conn.Domain != "" { args = append(args, "--domainName="+conn.Domain) }
		if conn.Username != "" { args = append(args, "--userName="+conn.Username) }
		cmd = exec.Command(bin, args...)

	case "citrix":
		homeDir, _ := os.UserHomeDir()
		_ = os.MkdirAll(homeDir+"/.ICAClient", 0700)
		_ = os.WriteFile(homeDir+"/.ICAClient/.eula_accepted", []byte(""), 0600)
		if strings.HasSuffix(conn.ServerURL, ".ica") || strings.HasPrefix(conn.ServerURL, "/") {
			cmd = exec.Command("/opt/Citrix/ICAClient/wfica", conn.ServerURL)
		} else {
			_ = exec.Command("/opt/Citrix/ICAClient/util/storebrowse", "-a", conn.ServerURL).Run()
			cmd = exec.Command("/opt/Citrix/ICAClient/selfservice")
		}

	case "dcv":
		cmd = exec.Command("/usr/bin/dcvviewer", conn.ServerURL)

	case "rdp":
		targetAddr := conn.ServerURL
		if !strings.Contains(targetAddr, ":") { targetAddr = targetAddr + ":3389" }
		tcpConn, err := net.DialTimeout("tcp", targetAddr, 2*time.Second)
		if err != nil {
			setLaunchStatus(fmt.Sprintf("RDP Error: Cannot reach %s (%v)", targetAddr, err))
			return nil
		}
		tcpConn.Close()

		args := []string{ 
			"/v:" + conn.ServerURL, 
			"/f", 
			"+floatbar", 
			"/network:auto", 
			"/rfx", 
			"/sound:sys:pulse", 
			"/microphone:sys:pulse", 
			"/cert:ignore", 
			"/dynamic-resolution"}
		if conn.AllMonitors { args = append(args, "/multimon") }
		if conn.Domain != "" { args = append(args, "/d:"+conn.Domain) }
		if conn.Username != "" { args = append(args, "/u:"+conn.Username) }
		if conn.Password != "" { args = append(args, "/p:"+conn.Password) } else { args = append(args, "-sec-nla") }
		if conn.SmartCardPassthrough { args = append(args, "/smartcard") }
		cmd = exec.Command("/usr/bin/xfreerdp3", args...)

	default:
		return fmt.Errorf("unknown app type: %s", conn.App)
	}

	cmd.Env = append(os.Environ(), "DISPLAY=:0")
	setLaunchStatus(fmt.Sprintf("Launching %s...", conn.Name))

	go func() {
		out, err := cmd.CombinedOutput()
		conn.Password = "" 
		if err != nil {
			outStr := strings.TrimSpace(string(out))
			if len(outStr) > 180 { outStr = outStr[len(outStr)-180:] }
			setLaunchStatus(fmt.Sprintf("Session ended (%s): %v | %s", conn.App, err, outStr))
		} else {
			setLaunchStatus(fmt.Sprintf("Session closed cleanly (%s)", conn.Name))
		}
	}()
	return nil
}

func main() {
	loadDefaultPolicy()
	go syncWithManagementServer()

	subFS, _ := fs.Sub(uiAssets, "ui")
	embeddedFS := http.FileServer(http.FS(subFS))

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		for _, liveDir := range []string{os.Getenv("UI_DIR"), "/mnt/dev/ui", "./ui"} {
			if liveDir == "" { continue }
			target := filepath.Join(liveDir, "index.html")
			if r.URL.Path != "/" && r.URL.Path != "/index.html" { target = filepath.Join(liveDir, filepath.Clean(r.URL.Path)) }
			if data, err := os.ReadFile(target); err == nil {
				if strings.HasSuffix(target, ".html") { w.Header().Set("Content-Type", "text/html; charset=utf-8")
				} else if strings.HasSuffix(target, ".css") { w.Header().Set("Content-Type", "text/css; charset=utf-8")
				} else if strings.HasSuffix(target, ".js") { w.Header().Set("Content-Type", "application/javascript; charset=utf-8") }
				w.Write(data)
				return
			}
		}
		embeddedFS.ServeHTTP(w, r)
	})

	http.HandleFunc("/api/policy", func(w http.ResponseWriter, r *http.Request) {
		policyMu.RLock()
		defer policyMu.RUnlock()
		lastLaunchMu.RLock()
		status := lastLaunchMsg
		lastLaunchMu.RUnlock()
		mgmtURLMu.RLock()
		currentSrv := mgmtServerURL
		mgmtURLMu.RUnlock()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{ "policy": currentPolicy, "status": status, "server_url": currentSrv })
	})

	http.HandleFunc("/api/config/server", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			var req struct { URL string `json:"url"` }
			json.NewDecoder(r.Body).Decode(&req)
			mgmtURLMu.Lock()
			mgmtServerURL = req.URL
			mgmtURLMu.Unlock()
			w.WriteHeader(http.StatusOK)
		}
	})

	http.HandleFunc("/api/launch", func(w http.ResponseWriter, r *http.Request) {
		var conn Connection
		json.NewDecoder(r.Body).Decode(&conn)
		launchVDIProcess(conn)
		w.WriteHeader(http.StatusOK)
	})

	http.HandleFunc("/api/hardware", func(w http.ResponseWriter, r *http.Request) {
		lsusbOut, _ := exec.Command("lsusb").Output()
		xrandrOut, _ := exec.Command("xrandr", "--query").Output()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{ "lsusb": string(lsusbOut), "xrandr": string(xrandrOut) })
	})

	http.HandleFunc("/api/usb-rules", func(w http.ResponseWriter, r *http.Request) {
		var rules []USBRule
		if json.NewDecoder(r.Body).Decode(&rules) == nil {
			policyMu.Lock()
			if currentPolicy.KioskMode.AllowLocalUSBConfig { currentPolicy.USBRules = rules }
			policyMu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
	})

	http.HandleFunc("/api/terminal", func(w http.ResponseWriter, r *http.Request) {
		setLaunchStatus("Opening Diagnostic Terminal...")
		// Explicit geometry forces it to appear on the primary monitor instead of getting lost
		cmd := exec.Command("/usr/bin/xterm", "-geometry", "140x40+100+100", "-fa", "Monospace", "-fs", "11")
		cmd.Env = append(os.Environ(), "DISPLAY=:0")
		go cmd.Run()
		w.WriteHeader(http.StatusOK)
	})

	log.Fatal(http.ListenAndServe("127.0.0.1:8080", nil))
}
