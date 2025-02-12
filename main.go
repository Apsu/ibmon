package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/progress"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// IBInterface represents a single monitored port on an InfiniBand adaptor.
type IBInterface struct {
	Adaptor  string // e.g. "mlx5_1"
	Port     string // e.g. "1", "2", etc.
	rxPath   string // path to the RX counter file
	txPath   string // path to the TX counter file
	ratePath string // path to the rate file
	prevRx   int64
	prevTx   int64
	maxGbps  float64 // parsed maximum bandwidth in Gbps
	rateStr  string  // display string (e.g. "400 Gbps (4X HDR)")
}

// readCounter reads a counter file and returns its value.
func readCounter(path string) (int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	s := strings.TrimSpace(string(data))
	return strconv.ParseInt(s, 10, 64)
}

// readRate reads the rate file (e.g. "400 Gb/sec (4X NDR)") and returns its trimmed content.
func readRate(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// parseRate extracts the maximum bandwidth (in Gbps) from a rate string.
// For example, given "400 Gb/sec (4X NDR)", it returns 400.
func parseRate(rateStr string) (float64, error) {
	fields := strings.Fields(rateStr)
	if len(fields) < 2 {
		return 0, fmt.Errorf("invalid rate string: %s", rateStr)
	}
	return strconv.ParseFloat(fields[0], 64)
}

// getInterfaces discovers all InfiniBand interfaces (across all ports) in /sys/class/infiniband.
// It returns a slice of IBInterface. The ignoreList maps adaptor names to skip.
func getInterfaces(ignoreList map[string]bool) ([]IBInterface, error) {
	basePath := "/sys/class/infiniband"
	adaptorEntries, err := os.ReadDir(basePath)
	if err != nil {
		return nil, err
	}

	var ifaces []IBInterface
	for _, entry := range adaptorEntries {
		adaptorName := entry.Name()
		if ignoreList[adaptorName] {
			continue
		}

		adaptorPath := filepath.Join(basePath, adaptorName)
		// Follow symlink and ensure it's a directory.
		fi, err := os.Stat(adaptorPath)
		if err != nil || !fi.IsDir() {
			continue
		}

		// Look inside the "ports" subdirectory.
		portsDir := filepath.Join(adaptorPath, "ports")
		portEntries, err := os.ReadDir(portsDir)
		if err != nil {
			continue
		}

		// For each port directory, create an IBInterface.
		for _, portEntry := range portEntries {
			if !portEntry.IsDir() {
				continue
			}
			portName := portEntry.Name() // e.g. "1", "2", etc.
			rxPath := filepath.Join(adaptorPath, "ports", portName, "counters", "port_rcv_data")
			txPath := filepath.Join(adaptorPath, "ports", portName, "counters", "port_xmit_data")
			ratePath := filepath.Join(adaptorPath, "ports", portName, "rate")

			// Ensure that both counter files exist.
			if _, err := os.Stat(rxPath); err != nil {
				continue
			}
			if _, err := os.Stat(txPath); err != nil {
				continue
			}

			// Read initial counters.
			prevRx, err := readCounter(rxPath)
			if err != nil {
				continue
			}
			prevTx, err := readCounter(txPath)
			if err != nil {
				continue
			}

			// Read and parse the rate file.
			rateFull, err := readRate(ratePath)
			var rateStr string
			var maxGbps float64
			if err == nil {
				// For a compact display, replace "Gb/sec" with "Gbps".
				rateStr = strings.Replace(rateFull, "Gb/sec", "Gbps", 1)
				maxGbps, err = parseRate(rateStr)
				if err != nil {
					maxGbps = 0
				}
			}

			iface := IBInterface{
				Adaptor:  adaptorName,
				Port:     portName,
				rxPath:   rxPath,
				txPath:   txPath,
				ratePath: ratePath,
				prevRx:   prevRx,
				prevTx:   prevTx,
				maxGbps:  maxGbps,
				rateStr:  rateStr,
			}
			ifaces = append(ifaces, iface)
		}
	}
	return ifaces, nil
}

// ifaceStatus holds the current throughput values for one IBInterface.
type ifaceStatus struct {
	iface   IBInterface
	rxValue float64 // current RX throughput (Gbps)
	txValue float64 // current TX throughput (Gbps)
}

// model is our Bubble Tea model.
type model struct {
	statuses  []ifaceStatus
	interval  time.Duration
	termWidth int // current terminal width
	vp        viewport.Model
}

// tickMsg is our message type for periodic ticks.
type tickMsg time.Time

// tick returns a command that sends a tickMsg after the given interval.
func tick(interval time.Duration) tea.Cmd {
	return tea.Tick(interval, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

// initialModel builds the initial model by discovering interfaces and initializing statuses.
func initialModel(interval time.Duration, ignoreList map[string]bool) (model, error) {
	ifaces, err := getInterfaces(ignoreList)
	if err != nil {
		return model{}, err
	}
	if len(ifaces) == 0 {
		return model{}, fmt.Errorf("no interfaces found")
	}
	var statuses []ifaceStatus
	for _, iface := range ifaces {
		statuses = append(statuses, ifaceStatus{
			iface:   iface,
			rxValue: 0,
			txValue: 0,
		})
	}
	// Create a default viewport. Its dimensions will be updated when a WindowSizeMsg is received.
	vp := viewport.New(80, 20)
	return model{
		statuses:  statuses,
		interval:  interval,
		termWidth: 80,
		vp:        vp,
	}, nil
}

// renderContent builds the main content to be displayed (all rows plus a footer with keybinds).
func (m model) renderContent() string {
	var s string
	// For each interface, build a row.
	for _, stat := range m.statuses {
		// Build the header.
		// Create the device:port string (e.g. "mlx5_0:1") and pad it to 10 characters.
		headerBase := fmt.Sprintf("%s:%s", stat.iface.Adaptor, stat.iface.Port)
		paddedHeader := fmt.Sprintf("%-10s", headerBase)
		// Append the rate in parentheses.
		header := fmt.Sprintf("%s (%s): ", paddedHeader, stat.iface.rateStr)
		headerWidth := lipgloss.Width(header)

		// Reserve fixed space for non-bar parts:
		// RX: "↑ " (2) + percentage (5) + " " (1) + throughput (11) = 19.
		// TX: "   ↓ " (5) + percentage (5) + " " (1) + throughput (11) = 22.
		const fixed = 19 + 22 // total 41
		available := m.termWidth - headerWidth - fixed
		if available < 10 {
			available = 10
		}
		barWidth := available / 2

		// Compute percentages for progress bars (saturate at 1.0).
		rxPct, txPct := 0.0, 0.0
		if stat.iface.maxGbps > 0 {
			rxPct = stat.rxValue / stat.iface.maxGbps
			if rxPct > 1.0 {
				rxPct = 1.0
			}
			txPct = stat.txValue / stat.iface.maxGbps
			if txPct > 1.0 {
				txPct = 1.0
			}
		}

		// Create new progress bar models with the computed width.
		rxBar := progress.New(progress.WithDefaultGradient(), progress.WithWidth(barWidth))
		txBar := progress.New(progress.WithDefaultGradient(), progress.WithWidth(barWidth))
		rxBar.SetPercent(rxPct)
		txBar.SetPercent(txPct)

		// Format the percentage and throughput values.
		rxPctStr := fmt.Sprintf("%4d%%", int(rxPct*100))
		txPctStr := fmt.Sprintf("%4d%%", int(txPct*100))
		// Use a throughput field that is 11 characters wide (e.g. "0000.0 Gbps")
		rxVal := fmt.Sprintf("%07.1f Gbps", stat.rxValue)
		txVal := fmt.Sprintf("%07.1f Gbps", stat.txValue)

		// Build the row:
		// [header] + "↑ " + [rxBar] + " " + [rxPctStr] + " " + [rxVal] + "   ↓ " + [txBar] + " " + [txPctStr] + " " + [txVal]
		line := header + fmt.Sprintf("↑ %s %s %s   ↓ %s %s %s", rxBar.View(), rxPctStr, rxVal, txBar.View(), txPctStr, txVal)
		s += line + "\n"
	}
	// Append a footer with key instructions.
	footer := "\n[q/ctrl+c to quit | ↑/↓ to scroll]"
	return s + footer
}

func (m model) Init() tea.Cmd {
	return tea.Batch(tick(m.interval))
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {

	case tickMsg:
		// For each interface, update counters and compute throughputs.
		for i, s := range m.statuses {
			currRx, err := readCounter(s.iface.rxPath)
			if err != nil {
				continue
			}
			currTx, err := readCounter(s.iface.txPath)
			if err != nil {
				continue
			}
			diffRx := currRx - s.iface.prevRx
			diffTx := currTx - s.iface.prevTx

			// Update previous counters.
			m.statuses[i].iface.prevRx = currRx
			m.statuses[i].iface.prevTx = currTx

			// Convert byte differences to Gbps: (bytes/s * 8) / 1e9.
			rxGbps := float64(diffRx) * 8 / 1e9 / m.interval.Seconds()
			txGbps := float64(diffTx) * 8 / 1e9 / m.interval.Seconds()
			m.statuses[i].rxValue = rxGbps
			m.statuses[i].txValue = txGbps
		}
		// Update the viewport content.
		m.vp.SetContent(m.renderContent())
		cmds = append(cmds, tick(m.interval))

	case tea.WindowSizeMsg:
		m.termWidth = msg.Width
		// Set viewport size: full width and leave 2 lines for padding.
		m.vp.Width = msg.Width
		m.vp.Height = msg.Height - 2
		m.vp.SetContent(m.renderContent())
		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		default:
			// Pass other key messages (like arrow keys) to the viewport.
			var cmd tea.Cmd
			m.vp, cmd = m.vp.Update(msg)
			cmds = append(cmds, cmd)
		}
	}
	// Also update the viewport with any messages.
	var vpCmd tea.Cmd
	m.vp, vpCmd = m.vp.Update(msg)
	cmds = append(cmds, vpCmd)

	return m, tea.Batch(cmds...)
}

func (m model) View() string {
	// Render the viewport content.
	return m.vp.View()
}

func main() {
	interval := flag.Duration("interval", 1*time.Second, "Update interval")
	ignoreFlag := flag.String("ignore", "", "Comma-separated list of adaptors to ignore")
	flag.Parse()
	ignoreMap := make(map[string]bool)
	if *ignoreFlag != "" {
		for _, name := range strings.Split(*ignoreFlag, ",") {
			ignoreMap[strings.TrimSpace(name)] = true
		}
	}

	m, err := initialModel(*interval, ignoreMap)
	if err != nil {
		log.Fatal(err)
	}

	// Use the alternate screen if desired; remove tea.WithAltScreen() to remain in the main terminal.
	p := tea.NewProgram(m, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		log.Fatal(err)
	}
}
