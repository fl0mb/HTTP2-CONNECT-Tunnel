package main

import (
	"bufio"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
)

// ParsePorts takes a string of ports and returns a sorted slice of unique integers.
// It supports formats like "80", "80,443", "80-90", "22,80,443,1000-2000".
// Attention: utils.go was vibecoded!
func ParsePorts(portStr string) ([]int, error) {
	ports := make(map[int]struct{})
	if portStr == "" {
		return []int{}, errors.New("no port specified")
	}
	parts := strings.Split(portStr, ",")

	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		if strings.Contains(part, "-") {
			rangeParts := strings.Split(part, "-")
			if len(rangeParts) != 2 {
				return nil, fmt.Errorf("invalid port range: %s", part)
			}

			startStr := strings.TrimSpace(rangeParts[0])
			endStr := strings.TrimSpace(rangeParts[1])

			start, err := strconv.Atoi(startStr)
			if err != nil {
				return nil, fmt.Errorf("invalid start port in range '%s': %w", part, err)
			}

			end, err := strconv.Atoi(endStr)
			if err != nil {
				return nil, fmt.Errorf("invalid end port in range '%s': %w", part, err)
			}

			if start > end {
				return nil, fmt.Errorf("invalid range: start port %d is greater than end port %d", start, end)
			}

			if start < 1 || end > 65535 {
				return nil, errors.New("port numbers must be between 1 and 65535")
			}

			for i := start; i <= end; i++ {
				ports[i] = struct{}{}
			}
		} else {
			port, err := strconv.Atoi(part)
			if err != nil {
				return nil, fmt.Errorf("invalid port number: %s", part)
			}

			if port < 1 || port > 65535 {
				return nil, fmt.Errorf("port number %d is out of valid range (1-65535)", port)
			}
			ports[port] = struct{}{}
		}
	}

	result := make([]int, 0, len(ports))
	for port := range ports {
		result = append(result, port)
	}

	sort.Ints(result)
	return result, nil
}

// parseAddresses takes a comma-separated string of IPs, hostnames, and CIDR notations,
// validates them, expands CIDRs, and returns a slice of targets.
// Attention: utils.go was vibecoded!
func parseAddresses(targetInput string) ([]string, error) {
	var targets []string
	// Split the input string by commas
	parts := strings.Split(targetInput, ",")

	for _, part := range parts {
		// Trim whitespace to handle inputs like "8.8.8.8, 4.4.4.4"
		trimmedPart := strings.TrimSpace(part)
		if trimmedPart == "" {
			continue // Skip empty entries
		}

		// Check if the part is a CIDR notation
		if strings.Contains(trimmedPart, "/") {
			ip, ipnet, err := net.ParseCIDR(trimmedPart)
			if err != nil {
				return nil, fmt.Errorf("invalid CIDR notation '%s': %w", trimmedPart, err)
			}

			// Iterate through all IPs in the CIDR range
			for ip := ip.Mask(ipnet.Mask); ipnet.Contains(ip); inc(ip) {
				targets = append(targets, ip.String())
			}
		} else {
			// It could be a single IP address or a hostname.
			// Let's see if it's a valid IP address literal first.
			ip := net.ParseIP(trimmedPart)
			if ip != nil {
				// It is a valid IP address. Ensure it's IPv4.
				if ip.To4() == nil {
					// It's a valid IP, but not IPv4 (e.g., IPv6)
					return nil, fmt.Errorf("not a valid IPv4 address: %s", trimmedPart)
				}
				targets = append(targets, ip.String())
			} else {
				// It's not a valid IP address, so we'll treat it as a hostname and add it directly.
				targets = append(targets, trimmedPart)
			}
		}
	}

	if len(targets) == 0 {
		return nil, fmt.Errorf("no valid targets (IPs, CIDR ranges, or hostnames) found in input")
	}

	return targets, nil
}

// inc increments an IP address. Used for iterating through a CIDR range.
func inc(ip net.IP) {
	for j := len(ip) - 1; j >= 0; j-- {
		ip[j]++
		if ip[j] > 0 {
			break
		}
	}
}

func readProxies(fileName string) []string {
	file, err := os.Open(fileName)
	if err != nil {
		log.Fatalf("failed to open file: %s", err)
	}
	defer file.Close()

	var proxies []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		proxies = append(proxies, scanner.Text())
	}

	// Check for errors during scanning.
	if err := scanner.Err(); err != nil {
		log.Fatalf("error during file scan: %s", err)
	}
	return proxies
}
