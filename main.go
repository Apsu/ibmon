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
)

// IBInterface represents a single monitored port on an InfiniBand adaptor.
type IBInterface struct {
	Adaptor  string // e.g. "mlx5_0"
	Port     string // e.g. "1", "2", etc.
	rxPath   string // path to the RX counter file
	txPath   string // path to the TX counter file
	ratePath string // path to the rate file
	prevRx   int64
	prevTx   int64
	maxGbps  float64 // parsed maximum bandwidth in Gbps
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

		portsDir := filepath.Join(adaptorPath, "ports")
		portEntries, err := os.ReadDir(portsDir)
		if err != nil {
			continue
		}

		for _, portEntry := range portEntries {
			if !portEntry.IsDir() {
				continue
			}
			portName := portEntry.Name() // e.g. "1", "2", etc.
			rxPath := filepath.Join(adaptorPath, "ports", portName, "counters", "port_rcv_data")
			txPath := filepath.Join(adaptorPath, "ports", portName, "counters", "port_xmit_data")
			ratePath := filepath.Join(adaptorPath, "ports", portName, "rate")

			// Both counter files must exist.
			if _, err := os.Stat(rxPath); err != nil {
				continue
			}
			if _, err := os.Stat(txPath); err != nil {
				continue
			}

			prevRx, err := readCounter(rxPath)
			if err != nil {
				continue
			}
			prevTx, err := readCounter(txPath)
			if err != nil {
				continue
			}

			// Read and parse the rate.
			rateFull, err := readRate(ratePath)
			var maxGbps float64
			if err == nil {
				// For compact display, replace "Gb/sec" with "Gbps" and parse the number.
				rateFull = strings.Replace(rateFull, "Gb/sec", "Gbps", 1)
				maxGbps, err = parseRate(rateFull)
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
	vp := viewport.New(80, 20)
	return model{
		statuses:  statuses,
		interval:  interval,
		termWidth: 80,
		vp:        vp,
	}, nil
}

// renderContent builds the content (all rows) to be displayed.
// Each row header is formatted as "mlx5_0:1 (200G): " in a fixed 18-character field.
func (m model) renderContent() string {
	var s string
	const headerFixedWidth = 18 // fixed width for header (device:port (speed))
	const fixed = 35            // total fixed width for non-bar parts after the header

	for _, stat := range m.statuses {
		// Format header as "mlx5_0:1 (200G): "
		headerBase := fmt.Sprintf("%s:%s", stat.iface.Adaptor, stat.iface.Port)
		paddedHeader := fmt.Sprintf("%-10s", headerBase)
		header := fmt.Sprintf("%s (%dG): ", paddedHeader, int(stat.iface.maxGbps))
		// Force the header to be exactly headerFixedWidth characters.
		if len(header) < headerFixedWidth {
			header = fmt.Sprintf("%-"+fmt.Sprintf("%d", headerFixedWidth)+"s", header)
		} else if len(header) > headerFixedWidth {
			header = header[:headerFixedWidth]
		}

		available := m.termWidth - headerFixedWidth - fixed
		if available < 10 {
			available = 10
		}
		barWidth := available / 2

		// Compute progress percentages (capped at 100%).
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

		// Create new progress bars with the computed width.
		rxBar := progress.New(progress.WithDefaultGradient(), progress.WithWidth(barWidth))
		txBar := progress.New(progress.WithDefaultGradient(), progress.WithWidth(barWidth))
		rxBar.SetPercent(rxPct)
		txBar.SetPercent(txPct)

		// Format percentage strings (5 characters, e.g. "  0%").
		rxPctStr := fmt.Sprintf("%4d%%", int(rxPct*100))
		txPctStr := fmt.Sprintf("%4d%%", int(txPct*100))
		// Format throughput in a 7-character field (e.g. "000.0G").
		rxVal := fmt.Sprintf("%06.1fG", stat.rxValue)
		txVal := fmt.Sprintf("%06.1fG", stat.txValue)

		// Build the row:
		// [header] + "↑ " + [rxBar] + " " + [rxPctStr] + " " + [rxVal] + "   ↓ " + [txBar] + " " + [txPctStr] + " " + [txVal]
		line := header + fmt.Sprintf("↑ %s %s %s   ↓ %s %s %s", rxBar.View(), rxPctStr, rxVal, txBar.View(), txPctStr, txVal)
		s += line + "\n"
	}
	return s
}

func (m model) Init() tea.Cmd {
	return tea.Batch(tick(m.interval))
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {

	case tickMsg:
		// Update throughput values for each interface.
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

			m.statuses[i].iface.prevRx = currRx
			m.statuses[i].iface.prevTx = currTx

			rxGbps := float64(diffRx) * 8 / 1e9 / m.interval.Seconds()
			txGbps := float64(diffTx) * 8 / 1e9 / m.interval.Seconds()
			m.statuses[i].rxValue = rxGbps
			m.statuses[i].txValue = txGbps
		}
		m.vp.SetContent(m.renderContent())
		cmds = append(cmds, tick(m.interval))

	case tea.WindowSizeMsg:
		m.termWidth = msg.Width
		m.vp.Width = msg.Width
		m.vp.Height = msg.Height - 1 // leave room if needed
		m.vp.SetContent(m.renderContent())
		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		default:
			var cmd tea.Cmd
			m.vp, cmd = m.vp.Update(msg)
			cmds = append(cmds, cmd)
		}
	}

	var vpCmd tea.Cmd
	m.vp, vpCmd = m.vp.Update(msg)
	cmds = append(cmds, vpCmd)

	return m, tea.Batch(cmds...)
}

func (m model) View() string {
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

	// Use the alternate screen; remove tea.WithAltScreen() if you prefer the normal terminal.
	p := tea.NewProgram(m, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		log.Fatal(err)
	}
}
