package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

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

type ThinClientNode struct {
	Hostname  string    `json:"hostname"`
	IPAddress string    `json:"ip_address"`
	Profile   string    `json:"profile"`
	LastSeen  time.Time `json:"last_seen"`
}

var (
	db            *sql.DB
	clientsMu     sync.Mutex
	activeClients = make(map[string]ThinClientNode)
)

func initDB() {
	var err error
	db, err = sql.Open("sqlite3", "./thin_management.db")
	if err != nil {
		log.Fatal(err)
	}

	db.Exec(`
		CREATE TABLE IF NOT EXISTS profiles (name TEXT PRIMARY KEY, data TEXT NOT NULL);
		CREATE TABLE IF NOT EXISTS assignments (hostname TEXT PRIMARY KEY, profile_name TEXT NOT NULL);
	`)

	// Always guarantee "Default Profile" exists
	defaultPol := `{"profile_name":"Default Profile","management_server_url":"http://10.0.2.2:9090","kiosk_mode":{"allow_ephemeral_connections":true,"allow_local_usb_config":true,"allow_local_display_audio_config":true,"auto_launch_default_connection":false},"enabled_apps":["horizon","citrix","dcv","rdp"],"default_connection_id":"conn-1","connections":[{"id":"conn-1","name":"Default VDI","app":"rdp","server_url":"10.0.2.2:3389","domain":"","username":"","all_monitors":true,"smartcard_passthrough":true}],"usb_rules":[]}`
	db.Exec("INSERT OR IGNORE INTO profiles (name, data) VALUES (?, ?)", "Default Profile", defaultPol)
}

func main() {
	initDB()
	http.Handle("/", http.FileServer(http.Dir("./ui")))

	// 1. Client Endpoint
	http.HandleFunc("/api/v1/client/policy", func(w http.ResponseWriter, r *http.Request) {
		hostname := r.URL.Query().Get("hostname")
		if hostname == "" {
			hostname = "unknown-client"
		}

		profileName := "Default Profile"
		db.QueryRow("SELECT profile_name FROM assignments WHERE hostname = ?", hostname).Scan(&profileName)

		clientsMu.Lock()
		activeClients[hostname] = ThinClientNode{Hostname: hostname, IPAddress: r.RemoteAddr, Profile: profileName, LastSeen: time.Now()}
		clientsMu.Unlock()

		var profileJson string
		err := db.QueryRow("SELECT data FROM profiles WHERE name = ?", profileName).Scan(&profileJson)
		if err != nil {
			log.Printf("Client %s requested profile '%s' but it was missing in DB!", hostname, profileName)
			http.Error(w, "Profile not found", 404)
			return
		}
		
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(profileJson))
	})

	// 2. Admin: Get active devices
	http.HandleFunc("/api/v1/admin/devices", func(w http.ResponseWriter, r *http.Request) {
		clientsMu.Lock()
		defer clientsMu.Unlock()
		now := time.Now()
		list := make([]ThinClientNode, 0)
		for k, client := range activeClients {
			if now.Sub(client.LastSeen) > 30*time.Second {
				delete(activeClients, k)
			} else {
				list = append(list, client)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(list)
	})

	// 3. Admin: Assign profile to device
	http.HandleFunc("/api/v1/admin/assign", func(w http.ResponseWriter, r *http.Request) {
		var req struct { Hostname string `json:"hostname"`; Profile string `json:"profile"` }
		json.NewDecoder(r.Body).Decode(&req)
		db.Exec("INSERT OR REPLACE INTO assignments (hostname, profile_name) VALUES (?, ?)", req.Hostname, req.Profile)
		w.WriteHeader(http.StatusOK)
	})

	// 4. Admin: Get all profiles
	http.HandleFunc("/api/v1/admin/profiles", func(w http.ResponseWriter, r *http.Request) {
		rows, _ := db.Query("SELECT data FROM profiles")
		defer rows.Close()
		var profiles []json.RawMessage
		for rows.Next() {
			var data string
			rows.Scan(&data)
			profiles = append(profiles, json.RawMessage(data))
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(profiles)
	})

	// 5. Admin: Save profile
	http.HandleFunc("/api/v1/admin/profile/save", func(w http.ResponseWriter, r *http.Request) {
		var pol Policy
		if err := json.NewDecoder(r.Body).Decode(&pol); err != nil {
			http.Error(w, fmt.Sprintf("Invalid JSON Payload: %v", err), 400)
			return
		}
		
		data, _ := json.Marshal(pol)
		_, err := db.Exec("INSERT OR REPLACE INTO profiles (name, data) VALUES (?, ?)", pol.ProfileName, string(data))
		if err != nil {
			http.Error(w, fmt.Sprintf("Database Error: %v", err), 500)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	log.Println("Management Server running on :9090")
	log.Fatal(http.ListenAndServe(":9090", nil))
}
