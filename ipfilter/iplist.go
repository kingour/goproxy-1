package ipfilter

import (
	"bufio"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"strings"

	logging "github.com/op/go-logging"
	"github.com/shell909090/goproxy/dns"
	"github.com/shell909090/goproxy/netutil"
)

var logger = logging.MustGetLogger("ipfilter")

var ErrDNSNotFound = errors.New("dns not found")

type IPFilter struct {
	rest []*net.IPNet
	idx1 map[byte][]*net.IPNet
	idx2 map[uint16][]*net.IPNet
}

func ListConatins(iplist []*net.IPNet, ip net.IP) bool {
	for _, ipnet := range iplist {
		if ipnet.Contains(ip) {
			logger.Debugf("%s matched %s.", ip.String(), ipnet.String())
			return true
		}
	}
	return false
}

func (f IPFilter) Contain(ip net.IP) bool {
	if x := ip.To4(); x != nil {
		ip = x
	}

	prefix2 := binary.BigEndian.Uint16(ip[:2])
	if iplist, ok := f.idx2[prefix2]; ok {
		if ListConatins(iplist, ip) {
			return true
		}
	}

	prefix1 := ip[0]
	if iplist, ok := f.idx1[prefix1]; ok {
		if ListConatins(iplist, ip) {
			return true
		}
	}

	if ListConatins(f.rest, ip) {
		return true
	}

	logger.Debugf("%s not match anything.", ip.String())
	return false
}

func ParseLine(line string) (ipnet *net.IPNet, err error) {
	_, ipnet, err = net.ParseCIDR(line)
	if err == nil {
		return
	}
	err = nil

	addrs := strings.Split(line, " ")

	ip := net.ParseIP(addrs[0])
	if x := ip.To4(); x != nil {
		ip = x
	}

	mask := net.ParseIP(addrs[1])
	if x := mask.To4(); x != nil {
		mask = x
	}

	ipnet = &net.IPNet{IP: ip, Mask: net.IPMask(mask)}
	return
}

func ReadIPList(f io.Reader) (filter *IPFilter, err error) {
	reader := bufio.NewReader(f)
	filter = &IPFilter{
		idx1: make(map[byte][]*net.IPNet),
		idx2: make(map[uint16][]*net.IPNet),
	}
	counter := 0

	var ipnet *net.IPNet
QUIT:
	for {
		line, err := reader.ReadString('\n')
		switch err {
		case io.EOF:
			if len(line) == 0 {
				break QUIT
			}
		case nil:
		default:
			logger.Error(err.Error())
			return nil, err
		}
		line = strings.Trim(line, "\r\n ")

		ipnet, err = ParseLine(line)
		if err != nil {
			logger.Error(err.Error())
			return nil, err
		}

		ones, _ := ipnet.Mask.Size()
		switch {
		case ones < 8:
			filter.rest = append(filter.rest, ipnet)
		case ones >= 8 && ones < 16:
			prefix := ipnet.IP[0]
			filter.idx1[prefix] = append(filter.idx1[prefix], ipnet)
		default:
			prefix := binary.BigEndian.Uint16(ipnet.IP[:2])
			filter.idx2[prefix] = append(filter.idx2[prefix], ipnet)
		}
		counter++
	}

	logger.Noticef(
		"blacklist loaded %d record(s), %d index1, %d index2 and %d no indexed.",
		counter, len(filter.idx1), len(filter.idx2), len(filter.rest))
	return
}

func ReadIPListFile(filename string) (filter *IPFilter, err error) {
	logger.Infof("load iplist from file %s.", filename)

	var f io.ReadCloser
	f, err = os.Open(filename)
	if err != nil {
		logger.Error(err.Error())
		return
	}
	defer f.Close()

	if strings.HasSuffix(filename, ".gz") {
		f, err = gzip.NewReader(f)
		if err != nil {
			logger.Error(err.Error())
			return
		}
	}

	return ReadIPList(f)
}

type FilterPair struct {
	dialer netutil.Dialer
	filter *IPFilter
}

type FilteredDialer struct {
	dialer netutil.Dialer
	dns.Resolver
	fps []*FilterPair
}

func NewFilteredDialer(dialer netutil.Dialer) (fd *FilteredDialer) {
	fd = &FilteredDialer{
		dialer:   dialer,
		Resolver: CreateDNSCache(),
	}
	return
}

func (fd *FilteredDialer) LoadFilter(dialer netutil.Dialer, filename string) (err error) {
	fp := &FilterPair{dialer: dialer}
	fp.filter, err = ReadIPListFile(filename)
	fd.fps = append(fd.fps, fp)
	return
}

func Getaddrs(resolver dns.Resolver, hostname string) (ips []net.IP) {
	ip := net.ParseIP(hostname)
	if ip != nil {
		ips = append(ips, ip)
		return
	}
	ips, err := resolver.LookupIP(hostname)
	if err != nil {
		logger.Error(err.Error())
	}
	return
}

func (fd *FilteredDialer) Dial(network, address string) (conn net.Conn, err error) {
	logger.Infof("filter dial: %s", address)
	if len(fd.fps) == 0 {
		return fd.dialer.Dial(network, address)
	}

	hostname, _, err := net.SplitHostPort(address)
	if err != nil {
		logger.Error(err.Error())
		return
	}

	addrs := Getaddrs(fd.Resolver, hostname)
	if addrs == nil {
		return nil, ErrDNSNotFound
	}

	for _, fp := range fd.fps {
		for _, addr := range addrs {
			if fp.filter.Contain(addr) {
				return fp.dialer.Dial(network, address)
			}
		}
	}

	return fd.dialer.Dial(network, address)
}
