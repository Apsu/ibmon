package main

import (
        "flag"
        "fmt"
        "log"
        "os"
        "strconv"
        "strings"
        "time"

        "github.com/bitfield/script"
)

// IBAdaptor holds information about an InfiniBand adaptor to monitor.
type IBAdaptor struct {
        Name     string
        rxPath   string
        txPath   string
        ratePath string
        prevRx   int64
        prevTx   int64
        portRate string // e.g., "400 Gbps"
}

// readCounter reads a sysfs counter file and converts its content to int64.
func readCounter(path string) (int64, error) {
        s := script.File(path)
        content, err := s.String()
        if err != nil {
                return 0, err
        }
        return strconv.ParseInt(strings.TrimSpace(content), 10, 64)
}

// readRate reads the port rate from the given file and returns a formatted string.
// For example, if the file contains: "400 Gb/sec (4X NDR)", this extracts "400 Gbps".
func readRate(path string) (string, error) {
        s := script.File(path)
        content, err := s.String()
        if err != nil {
                return "", err
        }
        content = strings.TrimSpace(content)
        fields := strings.Fields(content)
        if len(fields) >= 2 {
                // Replace "Gb/sec" with "Gbps" for a more compact header.
                rate := fmt.Sprintf("%s %s", fields[0], fields[1])
                rate = strings.Replace(rate, "Gb/sec", "Gbps", 1)
                return rate, nil
        }
        return content, nil
}

func main() {
        // Flags: polling interval and adaptors to ignore.
        interval := flag.Duration("interval", 1*time.Second, "Interval between readings")
        ignoreFlag := flag.String("ignore", "", "Comma-separated list of adaptors to ignore")
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
        entries, err := os.ReadDir(ibPath)
        if err != nil {
                log.Fatalf("Error reading %s: %v", ibPath, err)
        }

        var adaptors []IBAdaptor
        for _, entry := range entries {
                name := entry.Name()
                if ignoreMap[name] {
                        log.Printf("Ignoring adaptor %s", name)
                        continue
                }
                fullPath := fmt.Sprintf("%s/%s", ibPath, name)
                fi, err := os.Stat(fullPath) // follows symlinks
                if err != nil {
                        log.Printf("Error stating %s: %v", fullPath, err)
                        continue
                }
                if !fi.IsDir() {
                        log.Printf("Skipping %s: not a directory", fullPath)
                        continue
                }

                // Build file paths for port 1.
                rxPath := fmt.Sprintf("%s/%s/ports/1/counters/port_rcv_data", ibPath, name)
                txPath := fmt.Sprintf("%s/%s/ports/1/counters/port_xmit_data", ibPath, name)
                ratePath := fmt.Sprintf("%s/%s/ports/1/rate", ibPath, name)

                // Ensure counter files exist.
                if _, err := os.Stat(rxPath); err != nil {
                        log.Printf("Skipping adaptor %s, cannot access %s", name, rxPath)
                        continue
                }
                if _, err := os.Stat(txPath); err != nil {
                        log.Printf("Skipping adaptor %s, cannot access %s", name, txPath)
                        continue
                }

                prevRx, err := readCounter(rxPath)
                if err != nil {
                        log.Printf("Error reading RX counter for %s: %v", name, err)
                        continue
                }
                prevTx, err := readCounter(txPath)
                if err != nil {
                        log.Printf("Error reading TX counter for %s: %v", name, err)
                        continue
                }

                var portRate string
                if _, err := os.Stat(ratePath); err == nil {
                        portRate, err = readRate(ratePath)
                        if err != nil {
                                log.Printf("Error reading rate for %s: %v", name, err)
                                portRate = "?"
                        }
                } else {
                        portRate = "N/A"
                }

                adaptors = append(adaptors, IBAdaptor{
                        Name:     name,
                        rxPath:   rxPath,
                        txPath:   txPath,
                        ratePath: ratePath,
                        prevRx:   prevRx,
                        prevTx:   prevTx,
                        portRate: portRate,
                })
        }

        if len(adaptors) == 0 {
                log.Fatal("No InfiniBand adaptors found.")
        }

        // Print header: time and one column per adaptor.
        fmt.Printf("%-8s", "Time")
        for _, a := range adaptors {
                // Display the adaptor name and its reported rate (now in compact "Gbps" form).
                fmt.Printf(" | %-14s", fmt.Sprintf("%s(%s)", a.Name, a.portRate))
        }
        fmt.Println()
        fmt.Println(strings.Repeat("-", 8+len(adaptors)*18))

        // Main loop: on each interval, compute and display throughput.
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

                        // Convert bytes/s to Gbps:
                        // (bytes/s * 8) / 1e9 = Gbps.
                        rxGbps := float64(diffRx) * 8 / 1e9 / (*interval).Seconds()
                        txGbps := float64(diffTx) * 8 / 1e9 / (*interval).Seconds()

                        // Display with arrow symbols for RX (↑) and TX (↓) using 1 decimal.
                        fmt.Printf(" |↑ % 6.1f/↓ % 6.1f", rxGbps, txGbps)
                }
                fmt.Println()
        }
}
