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
)

// IBAdaptor holds information about an InfiniBand port to monitor.
type IBAdaptor struct {
	// Name is a composite of the adaptor name and port number, e.g. "mlx5_0.1"
	Name string
	// Full paths to the sysfs files.
	rxPath   string
	txPath   string
	ratePath string
	// Previous counter values to compute differences.
	prevRx int64
	prevTx int64
	// portRate is a human-readable rate (e.g., "400 Gbps").
	portRate string
}

// readCounter opens a file (whose content is expected to be an integer),
// trims whitespace, and returns its int64 value.
func readCounter(path string) (int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	s := strings.TrimSpace(string(data))
	return strconv.ParseInt(s, 10, 64)
}

// readRate reads the port rate from a file and massages it into a compact format.
func readRate(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(data))
	fields := strings.Fields(s)
	if len(fields) >= 2 {
		// For example, "400 Gb/sec" becomes "400 Gbps".
		rate := fmt.Sprintf("%s%s", fields[0], fields[1])
		return strings.Replace(rate, "Gb/sec", "G", 1), nil
	}
	return s, nil
}

// collectAdaptors walks the /sys/class/infiniband tree, looking for adaptor port files.
func collectAdaptors(ibPath string) ([]IBAdaptor, error) {
	var adaptors []IBAdaptor

	// List all entries in /sys/class/infiniband.
	adaptorEntries, err := os.ReadDir(ibPath)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", ibPath, err)
	}

	for _, adaptorEntry := range adaptorEntries {
		// The entries here might be symlinks; use os.Stat to follow them.
		adaptorPath := filepath.Join(ibPath, adaptorEntry.Name())
		info, err := os.Stat(adaptorPath)
		if err != nil {
			log.Printf("Unable to stat %s: %v", adaptorPath, err)
			continue
		}
		if !info.IsDir() {
			// Skip non-directories (or symlinks not pointing to directories).
			continue
		}

		adaptorName := adaptorEntry.Name()
		adaptorDir := adaptorPath // resolved adaptor directory

		// Look for the "ports" subdirectory.
		portsDir := filepath.Join(adaptorDir, "ports")
		portsEntries, err := os.ReadDir(portsDir)
		if err != nil {
			log.Printf("Skipping adaptor %s: cannot read ports directory: %v", adaptorName, err)
			continue
		}

		// Process each port directory.
		for _, portEntry := range portsEntries {
			// Typically these are real directories (or symlinks that resolve to directories).
			portPath := filepath.Join(portsDir, portEntry.Name())
			portInfo, err := os.Stat(portPath)
			if err != nil {
				log.Printf("Skipping port %s for adaptor %s: cannot stat port: %v", portEntry.Name(), adaptorName, err)
				continue
			}
			if !portInfo.IsDir() {
				continue
			}

			portName := portEntry.Name() // e.g. "1", "2", etc.
			// Build the full paths for the expected files.
			rxPath := filepath.Join(adaptorDir, "ports", portName, "counters", "port_rcv_data")
			txPath := filepath.Join(adaptorDir, "ports", portName, "counters", "port_xmit_data")
			ratePath := filepath.Join(adaptorDir, "ports", portName, "rate")

			// Ensure the RX and TX counter files exist.
			if _, err := os.Stat(rxPath); err != nil {
				if !os.IsNotExist(err) {
					log.Printf("Skipping %s port %s: cannot access %s: %v", adaptorName, portName, rxPath, err)
				}
				continue
			}
			if _, err := os.Stat(txPath); err != nil {
				if !os.IsNotExist(err) {
					log.Printf("Skipping %s port %s: cannot access %s: %v", adaptorName, portName, txPath, err)
				}
				continue
			}

			// Read initial counter values.
			prevRx, err := readCounter(rxPath)
			if err != nil {
				log.Printf("Skipping %s port %s: error reading RX counter: %v", adaptorName, portName, err)
				continue
			}
			prevTx, err := readCounter(txPath)
			if err != nil {
				log.Printf("Skipping %s port %s: error reading TX counter: %v", adaptorName, portName, err)
				continue
			}

			// Read the port rate if available.
			portRate := "N/A"
			if info, err := os.Stat(ratePath); err == nil && !info.IsDir() {
				if rate, err := readRate(ratePath); err == nil {
					portRate = rate
				} else {
					portRate = "?"
				}
			}

			// Create a composite name like "mlx5_0.1" (adaptor.port).
			compositeName := fmt.Sprintf("%s.%s", adaptorName, portName)
			adaptor := IBAdaptor{
				Name:     compositeName,
				rxPath:   rxPath,
				txPath:   txPath,
				ratePath: ratePath,
				prevRx:   prevRx,
				prevTx:   prevTx,
				portRate: portRate,
			}
			adaptors = append(adaptors, adaptor)
		}
	}

	return adaptors, nil
}

func main() {
	// Flags: polling interval and adaptors to ignore.
	interval := flag.Duration("interval", 1*time.Second, "Interval between readings")
	ignoreFlag := flag.String("ignore", "", "Comma-separated list of adaptor ports to ignore (e.g., mlx5_0.1,mlx5_0.2)")
	flag.Parse()

	// Build a map of adaptor names to ignore.
	ignoreMap := make(map[string]bool)
	if *ignoreFlag != "" {
		for _, name := range strings.Split(*ignoreFlag, ",") {
			if trimmed := strings.TrimSpace(name); trimmed != "" {
				ignoreMap[trimmed] = true
			}
		}
	}

	ibPath := "/sys/class/infiniband"
	adaptors, err := collectAdaptors(ibPath)
	if err != nil {
		log.Fatalf("Error collecting adaptors: %v", err)
	}
	if len(adaptors) == 0 {
		log.Fatal("No InfiniBand adaptors found.")
	}

	// Filter out adaptors specified in the ignore flag.
	var filtered []IBAdaptor
	for _, a := range adaptors {
		if ignoreMap[a.Name] {
			log.Printf("Ignoring adaptor %s", a.Name)
			continue
		}
		filtered = append(filtered, a)
	}
	adaptors = filtered

	// Print header: time and one column per adaptor port.
	fmt.Printf("%-8s", "Time")
	for _, a := range adaptors {
		fmt.Printf(" | %-14s", fmt.Sprintf("%s(%s)", a.Name, a.portRate))
	}
	fmt.Println()
	fmt.Println(strings.Repeat("-", 8+len(adaptors)*18))

	// Main polling loop.
	for {
		time.Sleep(*interval)
		timestamp := time.Now().Format("15:04:05")
		fmt.Printf("%-8s", timestamp)
		for i, a := range adaptors {
			currRx, err := readCounter(a.rxPath)
			if err != nil {
				log.Printf("Error reading RX for %s: %v", a.Name, err)
				continue
			}
			currTx, err := readCounter(a.txPath)
			if err != nil {
				log.Printf("Error reading TX for %s: %v", a.Name, err)
				continue
			}

			// Compute differences (bytes transferred during the interval).
			diffRx := currRx - a.prevRx
			diffTx := currTx - a.prevTx
			adaptors[i].prevRx = currRx
			adaptors[i].prevTx = currTx

			// Convert bytes/s to Gbps: (bytes/s * 8) / 1e9.
			rxGbps := float64(diffRx) * 8 / 1e9 / (*interval).Seconds()
			txGbps := float64(diffTx) * 8 / 1e9 / (*interval).Seconds()

			// Display with arrows for RX (↑) and TX (↓).
			fmt.Printf(" | ↑ % 3.1f/ ↓ % 3.1f", rxGbps, txGbps)
		}
		fmt.Println()
	}
}
