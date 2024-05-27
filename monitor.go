package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/rivo/tview"
	"golang.org/x/crypto/ssh"
)

type Node struct {
	IP       string `json:"ip"`
	Username string `json:"username"`
	Password string `json:"password"`
}

type Config struct {
	Nodes []Node `json:"nodes"`
}

// LogReader is an interface for reading logs from different Q execution methods
type LogReader interface {
	ReadLogs(session *ssh.Session) (string, error)
}

// ServiceLogReader reads logs from a running Q service
type ServiceLogReader struct {
	ServiceName string
}

func (s ServiceLogReader) ReadLogs(session *ssh.Session) (string, error) {
	cmd := fmt.Sprintf("journalctl -u %s.service -n 50 --no-hostname -o cat | grep -E '\"msg\":\"(connecting to bootstrap|broadcasting self-test info|peers in store)\"'", s.ServiceName)

	var b bytes.Buffer
	session.Stdout = &b
	if err := session.Run(cmd); err != nil {
		return "", fmt.Errorf("failed to run command '%s': %w", cmd, err)
	}

	return b.String(), nil
}

// TmuxLogReader reads logs from a tmux pane running Q
type TmuxLogReader struct {
	PaneName string
}

func (t TmuxLogReader) ReadLogs(session *ssh.Session) (string, error) {
	cmd := fmt.Sprintf("tmux capture-pane -t %s -pS -100 | grep -E '\"msg\":\"(connecting to bootstrap|broadcasting self-test info|peers in store)\"' | tail -n 200", t.PaneName)

	var b bytes.Buffer
	session.Stdout = &b
	if err := session.Run(cmd); err != nil {
		return "", fmt.Errorf("failed to run command '%s': %w", cmd, err)
	}

	return b.String(), nil
}

const configFileName = ".config.json"
const pollingInterval = 1 * time.Minute

// loadConfig loads node information from a config file
// the expected format matches the above structs, i.e.
// {"nodes": [{"ip":"...","username":"...","password":"..."},{...}]}
//
// do not use root as the user for this script. It's best to have a
// dedicated monitor user with the minimum required perms.
func loadConfig(filename string) (*Config, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	decoder := json.NewDecoder(file)
	config := &Config{}
	err = decoder.Decode(config)
	if err != nil {
		return nil, err
	}

	return config, nil
}

func main() {
	config, err := loadConfig(configFileName)
	if err != nil {
		log.Fatalf("Error loading config: %v", err)
	}

	// this is the definition of the view. Seems to run well
	// for up to 10 nodes on a laptop monitor, can probably
	// work for a few more on a desktop monitor, and you can also
	// run on multiple monitors with different node configs.
	app := tview.NewApplication()
	grid := tview.NewGrid().SetRows(0).SetColumns(0)
	textViews := make([]*tview.TextView, len(config.Nodes))
	for i, _ := range config.Nodes {
		textView := tview.NewTextView().
			SetDynamicColors(true).
			SetRegions(true).
			SetWrap(false)
		textViews[i] = textView
		grid.AddItem(textView, i/2, i%2, 1, 1, 0, 0, false)
	}

	var wg sync.WaitGroup
	go func() {
		for {
			for i, node := range config.Nodes {
				wg.Add(1)
				go func(i int, node Node) {
					defer wg.Done()
					// this implementation uses the service log reader, but you
					// can also use the tmux log reader (or add your own e.g. docker)
					logReader := ServiceLogReader{ServiceName: "ceremonyclient"}
					output, err := getNodeStatus(node, logReader)
					if err != nil {
						textViews[i].SetText(fmt.Sprintf("Error fetching status for node %s: %v", node.IP, err))
						app.QueueUpdateDraw(func() {
							textViews[i].SetText(fmt.Sprintf("Error fetching status for node %s: %v", node.IP, err))
						})
					} else {
						textViews[i].SetText(output)
						app.QueueUpdateDraw(func() {
							textViews[i].SetText(output)
						})
					}
				}(i, node)
			}
			wg.Wait()
			time.Sleep(pollingInterval)
		}
	}()

	if err := app.SetRoot(grid, true).Run(); err != nil {
		panic(err)
	}
}

func getNodeStatus(node Node, logReader LogReader) (string, error) {
	config := &ssh.ClientConfig{
		User: node.Username,
		Auth: []ssh.AuthMethod{
			ssh.Password(node.Password),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}

	conn, err := ssh.Dial("tcp", node.IP+":22", config)
	if err != nil {
		return "", fmt.Errorf("failed to dial: %w", err)
	}
	defer conn.Close()

	// commands for cpu, memory, disk space
	statsCommands := []string{
		"top -b -n 1 | grep 'Cpu(s)'",
		"free -m",
		"df -h /",
	}

	var stats []string
	for _, cmd := range statsCommands {
		session, err := conn.NewSession()
		if err != nil {
			return "", fmt.Errorf("failed to create session: %w", err)
		}
		defer session.Close()
		var b bytes.Buffer
		session.Stdout = &b
		if err := session.Run(cmd); err != nil {
			return "", fmt.Errorf("failed to run command '%s': %w", cmd, err)
		}

		stats = append(stats, b.String())
	}

	// we exec the logs command separately so we can use a reader
	session, err := conn.NewSession()
	if err != nil {
		return "", fmt.Errorf("failed to create session: %w", err)
	}
	defer session.Close()
	logs, err := logReader.ReadLogs(session)
	if err != nil {
		return "", fmt.Errorf("failed to read logs: %w", err)
	}
	stats = append(stats, logs)

	output := formatOutput(node.IP, stats)
	return output, nil
}

func formatOutput(ip string, stats []string) string {
	cpuUsage := parseCPUUsage(stats[0])
	memoryUsage := parseMemoryUsage(stats[1])

	output := fmt.Sprintf("[blue::b]Node: %s\n", ip)
	output += fmt.Sprintf("[green::b]CPU Usage: [white]%s\n", cpuUsage)
	output += fmt.Sprintf("[green::b]Memory Usage: [white]%s\n", memoryUsage)
	output += fmt.Sprintf("[green::b]Storage Usage:\n [white]%s", stats[2])

	logs := extractLogMessages(stats[3])
	output += fmt.Sprintf("[yellow::b]Logs: [white]%s", logs)

	return output
}

// extractLogMessages takes in a bunch of logs and returns the ones
// "we care about". I care about the three types included below, but
// you can add your own message keys if you want anything else to show up.
// If the log key isn't found in the last batch of logs it's omitted.
func extractLogMessages(logs string) string {
	var result strings.Builder

	lines := strings.Split(logs, "\n")
	messageTypes := map[string]map[string]interface{}{
		"connecting to bootstrap":     nil,
		"broadcasting self-test info": nil,
		"peers in store":              nil,
	}

	for _, line := range lines {
		var logEntry map[string]interface{}
		if err := json.Unmarshal([]byte(line), &logEntry); err != nil {
			continue
		}

		msg, ok := logEntry["msg"].(string)
		if !ok {
			continue
		}

		if _, exists := messageTypes[msg]; exists {
			messageTypes[msg] = logEntry
		}
	}

	for msg, logEntry := range messageTypes {
		if logEntry == nil {
			continue
		}

		// omit some keys that are not interesting
		delete(logEntry, "level")
		delete(logEntry, "ts")
		delete(logEntry, "caller")
		delete(logEntry, "msg")

		result.WriteString(fmt.Sprintf("{ msg: %v", msg))
		for key, value := range logEntry {
			if key != "msg" {
				switch v := value.(type) {
				case float64:
					result.WriteString(fmt.Sprintf("; %s: %.0f", key, v))
				case int, int64:
					result.WriteString(fmt.Sprintf("; %s: %d", key, v))
				default:
					result.WriteString(fmt.Sprintf("; %s: %v", key, value))
				}
			}
		}
		result.WriteString(" }\n")
	}

	return result.String()
}

func parseCPUUsage(cpuStat string) string {
	parts := strings.Fields(cpuStat)
	usage := fmt.Sprintf("User Space: %s%%; System Space: %s%%",
		parts[1], parts[3])
	return usage
}

func parseMemoryUsage(memStat string) string {
	lines := strings.Split(memStat, "\n")
	memParts := strings.Fields(lines[1])
	total := memParts[1]
	used := memParts[2]

	usage := fmt.Sprintf("Total Memory: %s MB; Used Memory: %s MB",
		total, used)
	return usage
}
