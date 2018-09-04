package tcp

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	logger "log"
	"net"
	"runtime/debug"
	"time"

	"github.com/snail007/goproxy/services"
	"github.com/snail007/goproxy/services/kcpcfg"
	"github.com/snail007/goproxy/utils"
	"github.com/snail007/goproxy/utils/jumper"
	"github.com/snail007/goproxy/utils/mapx"

	"strconv"
)

type TCPArgs struct {
	Parent              *string
	CertFile            *string
	KeyFile             *string
	CertBytes           []byte
	KeyBytes            []byte
	Local               *string
	ParentType          *string
	LocalType           *string
	Timeout             *int
	CheckParentInterval *int
	KCP                 kcpcfg.KCPConfigArgs
	Jumper              *string
}

type TCP struct {
	cfg       TCPArgs
	sc        *utils.ServerChannel
	isStop    bool
	userConns mapx.ConcurrentMap
	log       *logger.Logger
	jumper    *jumper.Jumper
}

func NewTCP() services.Service {
	return &TCP{
		cfg:       TCPArgs{},
		isStop:    false,
		userConns: mapx.NewConcurrentMap(),
	}
}
func (s *TCP) CheckArgs() (err error) {
	if len(*s.cfg.Parent) == 0 {
		err = fmt.Errorf("parent required for %s %s", *s.cfg.LocalType, *s.cfg.Local)
		return
	}
	if *s.cfg.ParentType == "" {
		err = fmt.Errorf("parent type unkown,use -T <tls|tcp|kcp|udp>")
		return
	}
	if *s.cfg.ParentType == "tls" || *s.cfg.LocalType == "tls" {
		s.cfg.CertBytes, s.cfg.KeyBytes, err = utils.TlsBytes(*s.cfg.CertFile, *s.cfg.KeyFile)
		if err != nil {
			return
		}
	}
	if *s.cfg.Jumper != "" {
		if *s.cfg.ParentType != "tls" && *s.cfg.ParentType != "tcp" {
			err = fmt.Errorf("jumper only worked of -T is tls or tcp")
			return
		}
		var j jumper.Jumper
		j, err = jumper.New(*s.cfg.Jumper, time.Millisecond*time.Duration(*s.cfg.Timeout))
		if err != nil {
			err = fmt.Errorf("parse jumper fail, err %s", err)
			return
		}
		s.jumper = &j
	}
	return
}
func (s *TCP) InitService() (err error) {

	return
}
func (s *TCP) StopService() {
	defer func() {
		e := recover()
		if e != nil {
			s.log.Printf("stop tcp service crashed,%s", e)
		} else {
			s.log.Printf("service tcp stoped")
		}
	}()
	s.isStop = true
	if s.sc.Listener != nil && *s.sc.Listener != nil {
		(*s.sc.Listener).Close()
	}
	if s.sc.UDPListener != nil {
		(*s.sc.UDPListener).Close()
	}
	for _, c := range s.userConns.Items() {
		(*c.(*net.Conn)).Close()
	}
}
func (s *TCP) Start(args interface{}, log *logger.Logger) (err error) {
	s.log = log
	s.cfg = args.(TCPArgs)
	if err = s.CheckArgs(); err != nil {
		return
	}
	if err = s.InitService(); err != nil {
		return
	}
	s.log.Printf("use %s parent %v", *s.cfg.ParentType, *s.cfg.Parent)
	host, port, _ := net.SplitHostPort(*s.cfg.Local)
	p, _ := strconv.Atoi(port)
	sc := utils.NewServerChannel(host, p, s.log)

	if *s.cfg.LocalType == "tcp" {
		err = sc.ListenTCP(s.callback)
	} else if *s.cfg.LocalType == "tls" {
		err = sc.ListenTls(s.cfg.CertBytes, s.cfg.KeyBytes, nil, s.callback)
	} else if *s.cfg.LocalType == "kcp" {
		err = sc.ListenKCP(s.cfg.KCP, s.callback, s.log)
	}
	if err != nil {
		return
	}
	s.log.Printf("%s proxy on %s", *s.cfg.LocalType, (*sc.Listener).Addr())
	s.sc = &sc
	return
}

func (s *TCP) Clean() {
	s.StopService()
}
func (s *TCP) callback(inConn net.Conn) {
	defer func() {
		if err := recover(); err != nil {
			s.log.Printf("%s conn handler crashed with err : %s \nstack: %s", *s.cfg.LocalType, err, string(debug.Stack()))
		}
	}()
	var err error
	lbAddr := ""
	switch *s.cfg.ParentType {
	case "kcp":
		fallthrough
	case "tcp":
		fallthrough
	case "tls":
		err = s.OutToTCP(&inConn)
	case "udp":
		err = s.OutToUDP(&inConn)
	default:
		err = fmt.Errorf("unkown parent type %s", *s.cfg.ParentType)
	}
	if err != nil {
		s.log.Printf("connect to %s parent %s fail, ERR:%s", *s.cfg.ParentType, lbAddr, err)
		utils.CloseConn(&inConn)
	}
}
func (s *TCP) OutToTCP(inConn *net.Conn) (err error) {

	var outConn net.Conn
	outConn, err = s.GetParentConn()
	if err != nil {
		s.log.Printf("connect to %s , err:%s", *s.cfg.Parent, err)
		utils.CloseConn(inConn)
		return
	}
	inAddr := (*inConn).RemoteAddr().String()
	//inLocalAddr := (*inConn).LocalAddr().String()
	outAddr := outConn.RemoteAddr().String()
	//outLocalAddr := outConn.LocalAddr().String()
	utils.IoBind((*inConn), outConn, func(err interface{}) {
		s.log.Printf("conn %s - %s released", inAddr, outAddr)
		s.userConns.Remove(inAddr)
	}, s.log)
	s.log.Printf("conn %s - %s connected", inAddr, outAddr)
	if c, ok := s.userConns.Get(inAddr); ok {
		(*c.(*net.Conn)).Close()
	}
	s.userConns.Set(inAddr, inConn)
	return
}
func (s *TCP) OutToUDP(inConn *net.Conn) (err error) {
	s.log.Printf("conn created , remote : %s ", (*inConn).RemoteAddr())
	for {
		if s.isStop {
			(*inConn).Close()
			return
		}
		srcAddr, body, err := utils.ReadUDPPacket(bufio.NewReader(*inConn))
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			//s.log.Printf("connection %s released", srcAddr)
			utils.CloseConn(inConn)
			break
		}
		//log.Debugf("udp packet revecived:%s,%v", srcAddr, body)
		dstAddr, err := net.ResolveUDPAddr("udp", *s.cfg.Parent)
		if err != nil {
			s.log.Printf("can't resolve address: %s", err)
			utils.CloseConn(inConn)
			break
		}
		clientSrcAddr := &net.UDPAddr{IP: net.IPv4zero, Port: 0}
		conn, err := net.DialUDP("udp", clientSrcAddr, dstAddr)
		if err != nil {
			s.log.Printf("connect to udp %s fail,ERR:%s", dstAddr.String(), err)
			continue
		}
		conn.SetDeadline(time.Now().Add(time.Millisecond * time.Duration(*s.cfg.Timeout)))
		_, err = conn.Write(body)
		if err != nil {
			s.log.Printf("send udp packet to %s fail,ERR:%s", dstAddr.String(), err)
			continue
		}
		//log.Debugf("send udp packet to %s success", dstAddr.String())
		buf := make([]byte, 512)
		len, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			s.log.Printf("read udp response from %s fail ,ERR:%s", dstAddr.String(), err)
			continue
		}
		respBody := buf[0:len]
		//log.Debugf("revecived udp packet from %s , %v", dstAddr.String(), respBody)
		_, err = (*inConn).Write(utils.UDPPacket(srcAddr, respBody))
		if err != nil {
			s.log.Printf("send udp response fail ,ERR:%s", err)
			utils.CloseConn(inConn)
			break
		}
		//s.log.Printf("send udp response success ,from:%s", dstAddr.String())
	}
	return

}
func (s *TCP) GetParentConn() (conn net.Conn, err error) {
	if *s.cfg.ParentType == "tls" {
		if s.jumper == nil {
			var _conn tls.Conn
			_conn, err = utils.TlsConnectHost(*s.cfg.Parent, *s.cfg.Timeout, s.cfg.CertBytes, s.cfg.KeyBytes, nil)
			if err == nil {
				conn = net.Conn(&_conn)
			}
		} else {
			conf, err := utils.TlsConfig(s.cfg.CertBytes, s.cfg.KeyBytes, nil)
			if err != nil {
				return nil, err
			}
			var _c net.Conn
			_c, err = s.jumper.Dial(*s.cfg.Parent, time.Millisecond*time.Duration(*s.cfg.Timeout))
			if err == nil {
				conn = net.Conn(tls.Client(_c, conf))
			}
		}

	} else if *s.cfg.ParentType == "kcp" {
		conn, err = utils.ConnectKCPHost(*s.cfg.Parent, s.cfg.KCP)
	} else {
		if s.jumper == nil {
			conn, err = utils.ConnectHost(*s.cfg.Parent, *s.cfg.Timeout)
		} else {
			conn, err = s.jumper.Dial(*s.cfg.Parent, time.Millisecond*time.Duration(*s.cfg.Timeout))
		}
	}
	return
}
