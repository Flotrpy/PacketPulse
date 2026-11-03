package geoip

import (
	"container/list"
	"context"
	"errors"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// Chain merges several providers field by field: for each field group
// (location, country, asn, isp, mobile) the first provider in priority order
// that has it wins. A provider error does not stop the chain; the first error
// is returned only if no provider had any data.
type Chain struct {
	Providers []Resolver
}

func (c *Chain) Name() string {
	names := make([]string, len(c.Providers))
	for i, p := range c.Providers {
		names[i] = p.Name()
	}
	return "chain(" + strings.Join(names, ",") + ")"
}

func (c *Chain) Lookup(ctx context.Context, ip netip.Addr) (Record, error) {
	if IsBogon(ip) {
		return Record{}, ErrBogon
	}
	var out Record
	var firstErr error
	any := false
	for _, p := range c.Providers {
		r, err := p.Lookup(ctx, ip)
		if err != nil {
			if ctx.Err() != nil {
				return Record{}, ctx.Err()
			}
			if firstErr == nil && !errors.Is(err, ErrNotFound) {
				firstErr = err
			}
			continue
		}
		any = true
		merge(&out, r)
	}
	if !any {
		if firstErr != nil {
			return Record{}, firstErr
		}
		return Record{}, ErrNotFound
	}
	out.IP = ip.Unmap().String()
	return out, nil
}

func merge(dst *Record, src Record) {
	if !dst.HasLocation && src.HasLocation {
		dst.Lat, dst.Lon, dst.HasLocation, dst.AccuracyKM = src.Lat, src.Lon, true, src.AccuracyKM
		dst.City, dst.Region = src.City, src.Region
		dst.source("location", src.Sources["location"])
	}
	if dst.CountryISO2 == "" {
		dst.CountryISO2 = src.CountryISO2
	}
	if dst.ASN == 0 && src.ASN != 0 {
		dst.ASN, dst.ASOrg = src.ASN, src.ASOrg
		dst.source("asn", src.Sources["asn"])
	}
	if dst.ISP == "" && src.ISP != "" {
		dst.ISP = src.ISP
		dst.source("isp", src.Sources["isp"])
	}
	if dst.MobileMCC == "" && src.MobileMCC != "" {
		dst.MobileMCC, dst.MobileMNC = src.MobileMCC, src.MobileMNC
		dst.source("mobile", src.Sources["mobile"])
	}
	if dst.Hostname == "" {
		dst.Hostname = src.Hostname
	}
	dst.Anycast = dst.Anycast || src.Anycast
}

// Cache wraps a Resolver with an LRU + TTL cache. Concurrent lookups for the
// same IP share one upstream call. Negative results (not found) are cached
// for NegativeTTL so unknown router IPs do not hammer paid APIs.
type Cache struct {
	Next        Resolver
	Size        int
	TTL         time.Duration
	NegativeTTL time.Duration

	mu       sync.Mutex
	lru      *list.List
	items    map[netip.Addr]*list.Element
	inflight map[netip.Addr]*call
	hits     int
	misses   int
}

type cacheEntry struct {
	ip      netip.Addr
	rec     Record
	err     error
	expires time.Time
}

type call struct {
	done chan struct{}
	rec  Record
	err  error
}

func NewCache(next Resolver, size int, ttl time.Duration) *Cache {
	return &Cache{Next: next, Size: size, TTL: ttl, NegativeTTL: ttl / 4}
}

func (c *Cache) Name() string { return "cache(" + c.Next.Name() + ")" }

func (c *Cache) Lookup(ctx context.Context, ip netip.Addr) (Record, error) {
	ip = ip.Unmap()
	if IsBogon(ip) {
		return Record{}, ErrBogon
	}
	c.mu.Lock()
	if c.items == nil {
		c.lru, c.items, c.inflight = list.New(), map[netip.Addr]*list.Element{}, map[netip.Addr]*call{}
	}
	if el, ok := c.items[ip]; ok {
		e := el.Value.(*cacheEntry)
		if time.Now().Before(e.expires) {
			c.lru.MoveToFront(el)
			c.hits++
			c.mu.Unlock()
			return e.rec, e.err
		}
		c.lru.Remove(el)
		delete(c.items, ip)
	}
	if cl, ok := c.inflight[ip]; ok {
		c.hits++
		c.mu.Unlock()
		select {
		case <-cl.done:
			return cl.rec, cl.err
		case <-ctx.Done():
			return Record{}, ctx.Err()
		}
	}
	cl := &call{done: make(chan struct{})}
	c.inflight[ip] = cl
	c.misses++
	c.mu.Unlock()

	cl.rec, cl.err = c.Next.Lookup(ctx, ip)
	close(cl.done)

	c.mu.Lock()
	delete(c.inflight, ip)
	ttl := c.TTL
	cacheable := cl.err == nil
	if errors.Is(cl.err, ErrNotFound) {
		ttl, cacheable = c.NegativeTTL, true
	}
	if cacheable && ttl > 0 {
		c.items[ip] = c.lru.PushFront(&cacheEntry{ip: ip, rec: cl.rec, err: cl.err, expires: time.Now().Add(ttl)})
		for c.Size > 0 && c.lru.Len() > c.Size {
			old := c.lru.Back()
			c.lru.Remove(old)
			delete(c.items, old.Value.(*cacheEntry).ip)
		}
	}
	c.mu.Unlock()
	return cl.rec, cl.err
}

// Stats returns cache hits and misses.
func (c *Cache) Stats() (hits, misses int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits, c.misses
}
