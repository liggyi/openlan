package _switch

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/luscis/openlan/pkg/api"
	co "github.com/luscis/openlan/pkg/config"
	"github.com/luscis/openlan/pkg/libol"
	cn "github.com/luscis/openlan/pkg/network"
	"github.com/vishvananda/netlink"
)

type Networker interface {
	String() string
	ID() string
	Initialize()
	Start(v api.Switcher)
	Stop()
	Bridge() cn.Bridger
	Config() *co.Network
	Subnet() string
	Reload(v api.Switcher)
	Provider() string
}

var workers = make(map[string]Networker)

func NewNetworker(c *co.Network) Networker {
	var obj Networker
	switch c.Provider {
	case "esp":
		obj = NewESPWorker(c)
	case "vxlan":
		obj = NewVxLANWorker(c)
	case "fabric":
		obj = NewFabricWorker(c)
	case "router":
		obj = NewRouterWorker(c)
	default:
		obj = NewOpenLANWorker(c)
	}
	workers[c.Name] = obj
	return obj
}

func GetWorker(name string) Networker {
	return workers[name]
}

func ListWorker(call func(w Networker)) {
	for _, worker := range workers {
		call(worker)
	}
}

type LinuxPort struct {
	name string // gre:xx, vxlan:xx
	vlan int
	link string
}

type WorkerImpl struct {
	uuid    string
	cfg     *co.Network
	out     *libol.SubLogger
	dhcp    *Dhcp
	outputs []*LinuxPort
	fire    *cn.FireWallTable
	setR    *cn.IPSet
	setV    *cn.IPSet
	vpn     *OpenVPN
}

func NewWorkerApi(c *co.Network) *WorkerImpl {
	return &WorkerImpl{
		cfg:  c,
		out:  libol.NewSubLogger(c.Name),
		setR: cn.NewIPSet(c.Name+"_r", "hash:net"),
		setV: cn.NewIPSet(c.Name+"_v", "hash:net"),
	}
}

func (w *WorkerImpl) Provider() string {
	return w.cfg.Provider
}

func (w *WorkerImpl) Initialize() {
	if w.cfg.Dhcp == "enable" {
		w.dhcp = NewDhcp(&co.Dhcp{
			Name:   w.cfg.Name,
			Subnet: w.cfg.Subnet,
			Bridge: w.cfg.Bridge,
		})
	}
	w.fire = cn.NewFireWallTable(w.cfg.Name)
	if out, err := w.setV.Clear(); err != nil {
		w.out.Error("WorkImpl.Initialize: create ipset: %s %s", out, err)
	}
	if out, err := w.setR.Clear(); err != nil {
		w.out.Error("WorkImpl.Initialize: create ipset: %s %s", out, err)
	}
}

func (w *WorkerImpl) AddPhysical(bridge string, vlan int, output string) {
	link, err := netlink.LinkByName(output)
	if err != nil {
		w.out.Error("WorkerImpl.LinkByName %s %s", output, err)
		return
	}
	slaver := output
	if vlan > 0 {
		if err := netlink.LinkSetUp(link); err != nil {
			w.out.Warn("WorkerImpl.LinkSetUp %s %s", output, err)
		}
		subLink := &netlink.Vlan{
			LinkAttrs: netlink.LinkAttrs{
				Name:        fmt.Sprintf("%s.%d", output, vlan),
				ParentIndex: link.Attrs().Index,
			},
			VlanId: vlan,
		}
		if err := netlink.LinkAdd(subLink); err != nil {
			w.out.Error("WorkerImpl.LinkAdd %s %s", subLink.Name, err)
			return
		}
		slaver = subLink.Name
	}
	br := cn.NewBrCtl(bridge, 0)
	if err := br.AddPort(slaver); err != nil {
		w.out.Warn("WorkerImpl.AddPhysical %s", err)
	}
}

func (w *WorkerImpl) AddOutput(bridge string, port *LinuxPort) {
	name := port.name
	values := strings.SplitN(name, ":", 6)
	if values[0] == "gre" {
		if port.link == "" {
			port.link = co.GenName("ge-")
		}
		link := &netlink.Gretap{
			LinkAttrs: netlink.LinkAttrs{
				Name: port.link,
			},
			Local:    libol.ParseAddr("0.0.0.0"),
			Remote:   libol.ParseAddr(values[1]),
			PMtuDisc: 1,
		}
		if err := netlink.LinkAdd(link); err != nil {
			w.out.Error("WorkerImpl.LinkAdd %s %s", name, err)
			return
		}
	} else if values[0] == "vxlan" {
		if len(values) < 3 {
			w.out.Error("WorkerImpl.LinkAdd %s wrong", name)
			return
		}
		if port.link == "" {
			port.link = co.GenName("vn-")
		}
		dport := 8472
		if len(values) == 4 {
			dport, _ = strconv.Atoi(values[3])
		}
		vni, _ := strconv.Atoi(values[2])
		link := &netlink.Vxlan{
			VxlanId: vni,
			LinkAttrs: netlink.LinkAttrs{
				TxQLen: -1,
				Name:   port.link,
			},
			Group: libol.ParseAddr(values[1]),
			Port:  dport,
		}
		if err := netlink.LinkAdd(link); err != nil {
			w.out.Error("WorkerImpl.LinkAdd %s %s", name, err)
			return
		}
	} else {
		port.link = name
	}
	w.out.Info("WorkerImpl.AddOutput %s %s", port.link, port.name)
	w.AddPhysical(bridge, port.vlan, port.link)
}

func (w *WorkerImpl) Start(v api.Switcher) {
	cfg := w.cfg
	fire := w.fire

	w.out.Info("WorkerImpl.Start")
	if cfg.Acl != "" {
		fire.Raw.Pre.AddRule(cn.IpRule{
			Input: cfg.Bridge.Name,
			Jump:  cfg.Acl,
		})
	}
	fire.Filter.For.AddRule(cn.IpRule{
		Input:  cfg.Bridge.Name,
		Output: cfg.Bridge.Name,
	})
	if cfg.Bridge.Mss > 0 {
		// forward to remote
		fire.Mangle.Post.AddRule(cn.IpRule{
			Output:  cfg.Bridge.Name,
			Proto:   "tcp",
			Match:   "tcp",
			TcpFlag: []string{"SYN,RST", "SYN"},
			Jump:    "TCPMSS",
			SetMss:  cfg.Bridge.Mss,
		})
		// connect from local
		fire.Mangle.In.AddRule(cn.IpRule{
			Input:   cfg.Bridge.Name,
			Proto:   "tcp",
			Match:   "tcp",
			TcpFlag: []string{"SYN,RST", "SYN"},
			Jump:    "TCPMSS",
			SetMss:  cfg.Bridge.Mss,
		})
	}
	for _, output := range cfg.Outputs {
		port := &LinuxPort{
			name: output.Interface,
			vlan: output.Vlan,
		}
		w.AddOutput(cfg.Bridge.Name, port)
		w.outputs = append(w.outputs, port)
	}
	if !(w.dhcp == nil) {
		w.dhcp.Start()
		fire.Nat.Post.AddRule(cn.IpRule{
			Source:  cfg.Bridge.Address,
			NoDest:  cfg.Bridge.Address,
			Jump:    cn.CMasq,
			Comment: "Default Gateway for DHCP",
		})
	}
	if !(w.vpn == nil) {
		w.vpn.Start()
	}
	w.fire.Start()
}

func (w *WorkerImpl) DelPhysical(bridge string, vlan int, output string) {
	if vlan > 0 {
		subLink := &netlink.Vlan{
			LinkAttrs: netlink.LinkAttrs{
				Name: fmt.Sprintf("%s.%d", output, vlan),
			},
		}
		if err := netlink.LinkDel(subLink); err != nil {
			w.out.Error("WorkerImpl.DelPhysical.LinkDel %s %s", subLink.Name, err)
			return
		}
	} else {
		br := cn.NewBrCtl(bridge, 0)
		if err := br.DelPort(output); err != nil {
			w.out.Warn("WorkerImpl.DelPhysical %s", err)
		}
	}
}

func (w *WorkerImpl) DelOutput(bridge string, port *LinuxPort) {
	w.out.Info("WorkerImpl.DelOutput %s %s", port.link, port.name)
	w.DelPhysical(bridge, port.vlan, port.link)
	values := strings.SplitN(port.name, ":", 6)
	if values[0] == "gre" {
		link := &netlink.Gretap{
			LinkAttrs: netlink.LinkAttrs{
				Name: port.link,
			},
		}
		if err := netlink.LinkDel(link); err != nil {
			w.out.Error("WorkerImpl.DelOutput.LinkDel %s %s", link.Name, err)
			return
		}
	} else if values[0] == "vxlan" {
		link := &netlink.Vxlan{
			LinkAttrs: netlink.LinkAttrs{
				Name: port.link,
			},
		}
		if err := netlink.LinkDel(link); err != nil {
			w.out.Error("WorkerImpl.DelOutput.LinkDel %s %s", link.Name, err)
			return
		}
	}
}

func (w *WorkerImpl) Stop() {
	w.out.Info("WorkerImpl.Stop")
	w.fire.Stop()
	if !(w.vpn == nil) {
		w.vpn.Stop()
	}
	if !(w.dhcp == nil) {
		w.dhcp.Stop()
	}
	for _, output := range w.outputs {
		w.DelOutput(w.cfg.Bridge.Name, output)
	}
	w.outputs = nil
	w.setR.Destroy()
	w.setV.Destroy()
}

func (w *WorkerImpl) String() string {
	return w.cfg.Name
}

func (w *WorkerImpl) ID() string {
	return w.uuid
}

func (w *WorkerImpl) Bridge() cn.Bridger {
	return nil
}

func (w *WorkerImpl) Config() *co.Network {
	return w.cfg
}

func (w *WorkerImpl) Subnet() string {
	return ""
}

func (w *WorkerImpl) Reload(v api.Switcher) {
}

func (w *WorkerImpl) toACL(acl, input string) {
	if input == "" {
		return
	}
	if acl != "" {
		w.fire.Raw.Pre.AddRule(cn.IpRule{
			Input: input,
			Jump:  acl,
		})
	}
}

func (w *WorkerImpl) openPort(protocol, port, comment string) {
	w.out.Info("WorkerImpl.openPort %s %s", protocol, port)
	// allowed forward between source and prefix.
	w.fire.Filter.In.AddRule(cn.IpRule{
		Proto:   protocol,
		Match:   "multiport",
		DstPort: port,
		Comment: comment,
	})
}

func (w *WorkerImpl) toForward_r(input, source, pfxSet, comment string) {
	w.out.Debug("WorkerImpl.toForward %s:%s %s:%s", input, source, pfxSet)
	// Allowed forward between source and prefix.
	w.fire.Filter.For.AddRule(cn.IpRule{
		Input:   input,
		Source:  source,
		DestSet: pfxSet,
		Comment: comment,
	})
}

func (w *WorkerImpl) toForward_s(input, srcSet, prefix, comment string) {
	w.out.Debug("WorkerImpl.toForward %s:%s %s:%s", input, srcSet, prefix)
	// Allowed forward between source and prefix.
	w.fire.Filter.For.AddRule(cn.IpRule{
		Input:   input,
		SrcSet:  srcSet,
		Dest:    prefix,
		Comment: comment,
	})
}

func (w *WorkerImpl) toMasq_r(source, pfxSet, comment string) {
	// Enable masquerade from source to prefix.
	w.fire.Nat.Post.AddRule(cn.IpRule{
		Source:  source,
		DestSet: pfxSet,
		Jump:    cn.CMasq,
		Comment: comment,
	})

}

func (w *WorkerImpl) toMasq_s(srcSet, prefix, comment string) {
	// Enable masquerade from source to prefix.
	w.fire.Nat.Post.AddRule(cn.IpRule{
		SrcSet:  srcSet,
		Dest:    prefix,
		Jump:    cn.CMasq,
		Comment: comment,
	})

}

func (w *WorkerImpl) toRelated(output, comment string) {
	w.out.Debug("WorkerImpl.toRelated %s", output)
	// Allowed forward between source and prefix.
	w.fire.Filter.For.AddRule(cn.IpRule{
		Output:  output,
		CtState: "RELATED,ESTABLISHED",
		Comment: comment,
	})
}
