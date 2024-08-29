package cache

import (
	"encoding/binary"
	"net"

	"github.com/luscis/openlan/pkg/libol"
	"github.com/luscis/openlan/pkg/models"
	"github.com/luscis/openlan/pkg/schema"
)

type network struct {
	Networks *libol.SafeStrMap
	UUID     *libol.SafeStrMap
	Addr     *libol.SafeStrMap
}

func (w *network) Add(n *models.Network) {
	libol.Debug("network.Add %v", *n)
	_ = w.Networks.Set(n.Name, n)
}

func (w *network) Del(name string) {
	libol.Debug("network.Del %s", name)
	w.Networks.Del(name)
}

func (w *network) Get(name string) *models.Network {
	if v := w.Networks.Get(name); v != nil {
		return v.(*models.Network)
	}
	return nil
}

func (w *network) List() <-chan *models.Network {
	c := make(chan *models.Network, 128)

	go func() {
		w.Networks.Iter(func(k string, v interface{}) {
			c <- v.(*models.Network)
		})
		c <- nil //Finish channel by nil.
	}()
	return c
}

func (w *network) ListLease() <-chan *schema.Lease {
	c := make(chan *schema.Lease, 128)

	go func() {
		w.UUID.Iter(func(k string, v interface{}) {
			c <- v.(*schema.Lease)
		})
		c <- nil //Finish channel by nil.
	}()
	return c
}

func (w *network) allocLease(sAddr, eAddr, network string) string {
	sIp := net.ParseIP(sAddr)
	eIp := net.ParseIP(eAddr)
	if sIp == nil || eIp == nil {
		return ""
	}
	start := binary.BigEndian.Uint32(sIp.To4()[:4])
	end := binary.BigEndian.Uint32(eIp.To4()[:4])
	for i := start; i <= end; i++ {
		tmp := make([]byte, 4)
		binary.BigEndian.PutUint32(tmp[:4], i)
		tmpStr := net.IP(tmp).String()
		if ok := w.GetLeaseByAddr(tmpStr, network); ok == nil {
			return tmpStr
		}
	}
	return ""
}

func (w *network) NewLease(alias, network string) *schema.Lease {
	n := w.Get(network)
	if n == nil || alias == "" {
		return nil
	}
	uuid := alias + "@" + network
	if obj, ok := w.UUID.GetEx(uuid); ok {
		l := obj.(*schema.Lease)
		return l // how to resolve conflict with new point?.
	}
	ipStr := w.allocLease(n.IpStart, n.IpEnd, network)
	if ipStr == "" {
		return nil
	}
	w.AddLease(alias, ipStr, network)
	return w.GetLease(alias, network)
}

func (w *network) GetLease(alias string, network string) *schema.Lease {
	uuid := alias + "@" + network
	if obj, ok := w.UUID.GetEx(uuid); ok {
		return obj.(*schema.Lease)
	}
	return nil
}

func (w *network) GetLeaseByAddr(addr string, network string) *schema.Lease {
	ruid := addr + "@" + network
	if obj, ok := w.Addr.GetEx(ruid); ok {
		return obj.(*schema.Lease)
	}
	return nil
}

func (w *network) AddLease(alias, ipStr, network string) *schema.Lease {
	if ipStr == "" || alias == "" {
		return nil
	}
	uuid := alias + "@" + network
	libol.Info("network.AddLease {%s %s}", uuid, ipStr)
	obj := &schema.Lease{
		Alias:   alias,
		Address: ipStr,
		Network: network,
	}
	if obj := w.UUID.Get(uuid); obj != nil {
		lease := obj.(*schema.Lease)
		ruid := lease.Address + "@" + network
		w.Addr.Del(ruid)
	}
	_ = w.UUID.Set(uuid, obj)
	ruid := ipStr + "@" + network
	_ = w.Addr.Set(ruid, obj)
	return obj
}

func (w *network) DelLease(alias string, network string) {
	uuid := alias + "@" + network
	libol.Debug("network.DelLease %s", uuid)
	addr := ""
	if obj, ok := w.UUID.GetEx(uuid); ok {
		lease := obj.(*schema.Lease)
		addr = lease.Address
		libol.Info("network.DelLease {%s %s} by UUID", uuid, addr)
		if lease.Type != "static" {
			w.UUID.Del(uuid)
		}
	}
	ruid := addr + "@" + network
	if obj, ok := w.Addr.GetEx(ruid); ok {
		lease := obj.(*schema.Lease)
		libol.Info("network.DelLease {%s %s} by Addr", ruid, alias)
		if lease.Type != "static" {
			w.Addr.Del(ruid)
		}
	}
}

var Network = network{
	Networks: libol.NewSafeStrMap(128),
	UUID:     libol.NewSafeStrMap(1024),
	Addr:     libol.NewSafeStrMap(1024),
}
