//go:build linux
// +build linux

package aghnet

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"strings"

	"github.com/AdguardTeam/AdGuardHome/internal/aghos"
	"github.com/AdguardTeam/golibs/errors"
	"github.com/google/renameio/maybe"
	"golang.org/x/sys/unix"
)

// dhcpcdStaticConfig checks if interface is configured by /etc/dhcpcd.conf to
// have a static IP.
func (n interfaceName) dhcpcdStaticConfig(r io.Reader) (subsources []string, cont bool, err error) {
	s := bufio.NewScanner(r)
	ifaceFound := findIfaceLine(s, string(n))
	if !ifaceFound {
		return nil, true, s.Err()
	}

	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		fields := strings.Fields(line)
		if len(fields) >= 2 &&
			fields[0] == "static" &&
			strings.HasPrefix(fields[1], "ip_address=") {
			return nil, false, s.Err()
		}

		if len(fields) > 0 && fields[0] == "interface" {
			// Another interface found.
			break
		}
	}

	return nil, true, s.Err()
}

// ifacesStaticConfig checks if the interface is configured by any file of
// /etc/network/interfaces format to have a static IP.
func (n interfaceName) ifacesStaticConfig(r io.Reader) (sub []string, cont bool, err error) {
	s := bufio.NewScanner(r)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if len(line) == 0 || line[0] == '#' {
			continue
		}

		// TODO(e.burkov): As man page interfaces(5) says, a line may be
		// extended across multiple lines by making the last character a
		// backslash.  Provide extended lines support.

		fields := strings.Fields(line)
		fieldsNum := len(fields)

		// Man page interfaces(5) declares that interface definition
		// should consist of the key word "iface" followed by interface
		// name, and method at fourth field.
		if fieldsNum >= 4 &&
			fields[0] == "iface" && fields[1] == string(n) && fields[3] == "static" {
			return nil, false, nil
		}

		if fieldsNum >= 2 && fields[0] == "source" {
			sub = append(sub, fields[1])
		}
	}

	return sub, true, s.Err()
}

func ifaceHasStaticIP(ifaceName string) (has bool, err error) {
	// TODO(a.garipov): Currently, this function returns the first
	// definitive result.  So if /etc/dhcpcd.conf has a static IP while
	// /etc/network/interfaces doesn't, it will return true.  Perhaps this
	// is not the most desirable behavior.

	iface := interfaceName(ifaceName)

	for _, pair := range []struct {
		aghos.FileWalker
		filename string
	}{{
		FileWalker: iface.dhcpcdStaticConfig,
		filename:   "/etc/dhcpcd.conf",
	}, {
		FileWalker: iface.ifacesStaticConfig,
		filename:   "/etc/network/interfaces",
	}} {
		has, err = pair.Walk(pair.filename)
		if err != nil {
			return false, err
		}

		if has {
			return true, nil
		}
	}

	return false, ErrNoStaticIPInfo
}

func canBindPrivilegedPorts() (can bool, err error) {
	cnbs, err := unix.PrctlRetInt(unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_IS_SET, unix.CAP_NET_BIND_SERVICE, 0, 0)
	// Don't check the error because it's always nil on Linux.
	adm, _ := aghos.HaveAdminRights()

	return cnbs == 1 || adm, err
}

// findIfaceLine scans s until it finds the line that declares an interface with
// the given name.  If findIfaceLine can't find the line, it returns false.
func findIfaceLine(s *bufio.Scanner, name string) (ok bool) {
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "interface" && fields[1] == name {
			return true
		}
	}

	return false
}

// ifaceSetStaticIP configures the system to retain its current IP on the
// interface through dhcpdc.conf.
func ifaceSetStaticIP(ifaceName string) (err error) {
	ipNet := GetSubnet(ifaceName)
	if ipNet.IP == nil {
		return errors.Error("can't get IP address")
	}

	gatewayIP := GatewayIP(ifaceName)
	add := dhcpcdConfIface(ifaceName, ipNet, gatewayIP, ipNet.IP)

	body, err := os.ReadFile("/etc/dhcpcd.conf")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	body = append(body, []byte(add)...)
	err = maybe.WriteFile("/etc/dhcpcd.conf", body, 0o644)
	if err != nil {
		return fmt.Errorf("writing conf: %w", err)
	}

	return nil
}

// dhcpcdConfIface returns configuration lines for the dhcpdc.conf files that
// configure the interface to have a static IP.
func dhcpcdConfIface(ifaceName string, ipNet *net.IPNet, gatewayIP, dnsIP net.IP) (conf string) {
	var body []byte

	add := fmt.Sprintf(
		"\n# %[1]s added by AdGuard Home.\ninterface %[1]s\nstatic ip_address=%s\n",
		ifaceName,
		ipNet)
	body = append(body, []byte(add)...)

	if gatewayIP != nil {
		add = fmt.Sprintf("static routers=%s\n", gatewayIP)
		body = append(body, []byte(add)...)
	}

	add = fmt.Sprintf("static domain_name_servers=%s\n\n", dnsIP)
	body = append(body, []byte(add)...)

	return string(body)
}
