// Copyright 2019, Chef.  All rights reserved.
// https://github.com/q191201771/lal
//
// Use of this source code is governed by a MIT-style license
// that can be found in the License file.
//
// Author: Chef (191201771@qq.com)

package logic

import (
	"fmt"
	"sync"
	"time"

	"github.com/q191201771/lal/pkg/hls"

	"github.com/q191201771/lal/pkg/base"

	"github.com/q191201771/lal/pkg/httpts"

	"github.com/q191201771/lal/pkg/rtsp"

	"github.com/q191201771/lal/pkg/httpflv"
	"github.com/q191201771/lal/pkg/rtmp"
	"github.com/q191201771/naza/pkg/nazalog"
)

type ServerManager struct {
	httpServerManager *base.HTTPServerManager
	httpServerHandler *HTTPServerHandler
	hlsServerHandler  *hls.ServerHandler

	rtmpServer    *rtmp.Server
	rtspServer    *rtsp.Server
	httpAPIServer *HTTPAPIServer
	exitChan      chan struct{}

	mutex    sync.Mutex
	groupMap map[string]*Group // TODO chef: with appName
}

func NewServerManager() *ServerManager {
	m := &ServerManager{
		groupMap: make(map[string]*Group),
		exitChan: make(chan struct{}),
	}

	if config.HTTPFLVConfig.Enable || config.HTTPFLVConfig.EnableHTTPS ||
		config.HTTPTSConfig.Enable || config.HTTPTSConfig.EnableHTTPS ||
		config.HLSConfig.Enable || config.HLSConfig.EnableHTTPS {
		m.httpServerManager = base.NewHTTPServerManager()
		m.httpServerHandler = NewHTTPServerHandler(m)
		m.hlsServerHandler = hls.NewServerHandler(config.HLSConfig.OutPath)
	}

	if config.RTMPConfig.Enable {
		m.rtmpServer = rtmp.NewServer(m, config.RTMPConfig.Addr,"",false)
	}
	// if config.RTSPConfig.Enable {
	// 	m.rtspServer = rtsp.NewServer(config.RTSPConfig.Addr, m)
	// }
	// if config.HTTPAPIConfig.Enable {
	// 	m.httpAPIServer = NewHTTPAPIServer(config.HTTPAPIConfig.Addr, m)
	// }
	return m
}

func (sm *ServerManager) RunLoop() error {
	HttpNotify.OnServerStart()

	var addMux = func(config CommonHTTPServerConfig, handler base.Handler, name string) error {
		if config.Enable {
			err := sm.httpServerManager.AddListen(
				base.LocalAddrCtx{Addr: config.HTTPListenAddr},
				config.URLPattern,
				handler,
			)
			if err != nil {
				nazalog.Infof("add http listen for %s failed. addr=%s, pattern=%s, err=%+v", name, config.HTTPListenAddr, config.URLPattern, err)
				return err
			}
			nazalog.Infof("add http listen for %s. addr=%s, pattern=%s", name, config.HTTPListenAddr, config.URLPattern)
		}
		if config.EnableHTTPS {
			err := sm.httpServerManager.AddListen(
				base.LocalAddrCtx{IsHTTPS: true, Addr: config.HTTPSListenAddr, CertFile: config.HTTPSCertFile, KeyFile: config.HTTPSKeyFile},
				config.URLPattern,
				handler,
			)
			if err != nil {
				nazalog.Infof("add https listen for %s failed. addr=%s, pattern=%s, err=%+v", name, config.HTTPListenAddr, config.URLPattern, err)
				return err
			}
			nazalog.Infof("add https listen for %s. addr=%s, pattern=%s", name, config.HTTPSListenAddr, config.URLPattern)
		}
		return nil
	}

	if err := addMux(config.HTTPFLVConfig.CommonHTTPServerConfig, sm.httpServerHandler.ServeSubSession, "httpflv"); err != nil {
		return err
	}
	if err := addMux(config.HTTPTSConfig.CommonHTTPServerConfig, sm.httpServerHandler.ServeSubSession, "httpts"); err != nil {
		return err
	}
	if err := addMux(config.HLSConfig.CommonHTTPServerConfig, sm.hlsServerHandler.ServeHTTP, "hls"); err != nil {
		return err
	}

	go func() {
		if err := sm.httpServerManager.RunLoop(); err != nil {
			nazalog.Error(err)
		}
	}()

	if sm.rtmpServer != nil {
		if err := sm.rtmpServer.Listen(); err != nil {
			return err
		}
		go func() {
			if err := sm.rtmpServer.RunLoop(); err != nil {
				nazalog.Error(err)
			}
		}()
	}

	// if sm.rtspServer != nil {
	// 	if err := sm.rtspServer.Listen(); err != nil {
	// 		return err
	// 	}
	// 	go func() {
	// 		if err := sm.rtspServer.RunLoop(); err != nil {
	// 			nazalog.Error(err)
	// 		}
	// 	}()
	// }

	// if sm.httpAPIServer != nil {
	// 	if err := sm.httpAPIServer.Listen(); err != nil {
	// 		return err
	// 	}
	// 	go func() {
	// 		if err := sm.httpAPIServer.Runloop(); err != nil {
	// 			nazalog.Error(err)
	// 		}
	// 	}()
	// }

	uis := uint32(config.HTTPNotifyConfig.UpdateIntervalSec)
	var updateInfo base.UpdateInfo
	updateInfo.ServerID = config.ServerID
	updateInfo.Groups = sm.statAllGroup()
	HttpNotify.OnUpdate(updateInfo)

	t := time.NewTicker(1 * time.Second)
	defer t.Stop()
	var count uint32
	for {
		select {
		case <-sm.exitChan:
			return nil
		case <-t.C:
			count++

			sm.iterateGroup()

			if (count % 30) == 0 {
				sm.mutex.Lock()
				nazalog.Debugf("group size=%d", len(sm.groupMap))
				// only for debug
				if len(sm.groupMap) < 10 {
					for _, g := range sm.groupMap {
						nazalog.Debugf("%s", g.StringifyDebugStats())
					}
				}
				sm.mutex.Unlock()
			}

			if uis != 0 && (count%uis) == 0 {
				updateInfo.ServerID = config.ServerID
				updateInfo.Groups = sm.statAllGroup()
				HttpNotify.OnUpdate(updateInfo)
			}
		}
	}

	// never reach here
}

func (sm *ServerManager) Dispose() {
	nazalog.Debug("dispose server manager.")

	// TODO(chef) add httpServer

	if sm.rtmpServer != nil {
		sm.rtmpServer.Dispose()
	}
	//if sm.hlsServer != nil {
	//	sm.hlsServer.Dispose()
	//}

	sm.mutex.Lock()
	for _, group := range sm.groupMap {
		group.Dispose()
	}
	sm.mutex.Unlock()

	sm.exitChan <- struct{}{}
}

func (sm *ServerManager) GetGroup(appName string, streamName string) *Group {
	nazalog.Debugf("getgroup")
	sm.mutex.Lock()
	defer sm.mutex.Unlock()

	return sm.getGroup(appName, streamName)
}

// ServerObserver of rtmp.Server
func (sm *ServerManager) OnRTMPConnect(session *rtmp.ServerSession, opa rtmp.ObjectPairArray) {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()

	var info base.RTMPConnectInfo
	info.ServerID = config.ServerID
	info.SessionID = session.UniqueKey()
	//info.RemoteAddr = session.GetStat().RemoteAddr
	if app, err := opa.FindString("app"); err == nil {
		info.App = app
	}
	if flashVer, err := opa.FindString("flashVer"); err == nil {
		info.FlashVer = flashVer
	}
	if tcURL, err := opa.FindString("tcUrl"); err == nil {
		info.TCURL = tcURL
	}
	HttpNotify.OnRTMPConnect(info)
}

// ServerObserver of rtmp.Server
func (sm *ServerManager) OnNewRTMPPubSession(session *rtmp.ServerSession) bool {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()
	group := sm.getOrCreateGroup(session.AppName(), session.StreamName())
	res := group.AddRTMPPubSession(session)

	// TODO chef: res值为false时，可以考虑不回调
	// TODO chef: 每次赋值都逐个拼，代码冗余，考虑直接用ISession抽离一下代码
	var info base.PubStartInfo
	info.ServerID = config.ServerID
	info.Protocol = base.ProtocolRTMP
	info.URL = session.URL()
	info.AppName = session.AppName()
	info.StreamName = session.StreamName()
	info.URLParam = session.RawQuery()
	info.SessionID = session.UniqueKey()
	//info.RemoteAddr = session.GetStat().RemoteAddr
	info.HasInSession = group.HasInSession()
	info.HasOutSession = group.HasOutSession()
	HttpNotify.OnPubStart(info)
	return res
}

// ServerObserver of rtmp.Server
func (sm *ServerManager) OnDelRTMPPubSession(session *rtmp.ServerSession) {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()
	group := sm.getGroup(session.AppName(), session.StreamName())
	if group == nil {
		return
	}

	group.DelRTMPPubSession(session)

	var info base.PubStopInfo
	info.ServerID = config.ServerID
	info.Protocol = base.ProtocolRTMP
	info.URL = session.URL()
	info.AppName = session.AppName()
	info.StreamName = session.StreamName()
	info.URLParam = session.RawQuery()
	info.SessionID = session.UniqueKey()
	//info.RemoteAddr = session.GetStat().RemoteAddr
	info.HasInSession = group.HasInSession()
	info.HasOutSession = group.HasOutSession()
	HttpNotify.OnPubStop(info)
}

// ServerObserver of rtmp.Server
func (sm *ServerManager) OnNewRTMPSubSession(session *rtmp.ServerSession) bool {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()
	group := sm.getOrCreateGroup(session.AppName(), session.StreamName())
	group.AddRTMPSubSession(session)

	var info base.SubStartInfo
	info.ServerID = config.ServerID
	info.Protocol = base.ProtocolRTMP
	info.Protocol = session.URL()
	info.AppName = session.AppName()
	info.StreamName = session.StreamName()
	info.URLParam = session.RawQuery()
	info.SessionID = session.UniqueKey()
	//info.RemoteAddr = session.GetStat().RemoteAddr
	info.HasInSession = group.HasInSession()
	info.HasOutSession = group.HasOutSession()
	HttpNotify.OnSubStart(info)

	return true
}

// ServerObserver of rtmp.Server
func (sm *ServerManager) OnDelRTMPSubSession(session *rtmp.ServerSession) {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()
	group := sm.getGroup(session.AppName(), session.StreamName())
	if group == nil {
		return
	}

	group.DelRTMPSubSession(session)

	var info base.SubStopInfo
	info.ServerID = config.ServerID
	info.Protocol = base.ProtocolRTMP
	info.AppName = session.AppName()
	info.StreamName = session.StreamName()
	info.URLParam = session.RawQuery()
	info.SessionID = session.UniqueKey()
	//info.RemoteAddr = session.GetStat().RemoteAddr
	info.HasInSession = group.HasInSession()
	info.HasOutSession = group.HasOutSession()
	HttpNotify.OnSubStop(info)
}

// ServerObserver of httpflv.Server
func (sm *ServerManager) OnNewHTTPFLVSubSession(session *httpflv.SubSession) bool {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()
	group := sm.getOrCreateGroup(session.AppName(), session.StreamName())
	group.AddHTTPFLVSubSession(session)

	var info base.SubStartInfo
	info.ServerID = config.ServerID
	info.Protocol = base.ProtocolHTTPFLV
	info.URL = session.URL()
	info.AppName = session.AppName()
	info.StreamName = session.StreamName()
	info.URLParam = session.RawQuery()
	info.SessionID = session.UniqueKey()
	//info.RemoteAddr = session.GetStat().RemoteAddr
	info.HasInSession = group.HasInSession()
	info.HasOutSession = group.HasOutSession()
	HttpNotify.OnSubStart(info)
	return true
}

// ServerObserver of httpflv.Server
func (sm *ServerManager) OnDelHTTPFLVSubSession(session *httpflv.SubSession) {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()
	group := sm.getGroup(session.AppName(), session.StreamName())
	if group == nil {
		return
	}

	group.DelHTTPFLVSubSession(session)

	var info base.SubStopInfo
	info.ServerID = config.ServerID
	info.Protocol = base.ProtocolHTTPFLV
	info.URL = session.URL()
	info.AppName = session.AppName()
	info.StreamName = session.StreamName()
	info.URLParam = session.RawQuery()
	info.SessionID = session.UniqueKey()
	//info.RemoteAddr = session.GetStat().RemoteAddr
	info.HasInSession = group.HasInSession()
	info.HasOutSession = group.HasOutSession()
	HttpNotify.OnSubStop(info)
}

// ServerObserver of httpts.Server
func (sm *ServerManager) OnNewHTTPTSSubSession(session *httpts.SubSession) bool {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()
	group := sm.getOrCreateGroup(session.AppName(), session.StreamName())
	group.AddHTTPTSSubSession(session)

	var info base.SubStartInfo
	info.ServerID = config.ServerID
	info.Protocol = base.ProtocolHTTPTS
	info.URL = session.URL()
	info.AppName = session.AppName()
	info.StreamName = session.StreamName()
	info.URLParam = session.RawQuery()
	info.SessionID = session.UniqueKey()
	///info.RemoteAddr = session.GetStat().RemoteAddr
	info.HasInSession = group.HasInSession()
	info.HasOutSession = group.HasOutSession()
	HttpNotify.OnSubStart(info)
	return true
}

// ServerObserver of httpts.Server
func (sm *ServerManager) OnDelHTTPTSSubSession(session *httpts.SubSession) {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()
	group := sm.getGroup(session.AppName(), session.StreamName())
	if group == nil {
		return
	}

	group.DelHTTPTSSubSession(session)

	var info base.SubStopInfo
	info.ServerID = config.ServerID
	info.Protocol = base.ProtocolHTTPTS
	info.URL = session.URL()
	info.AppName = session.AppName()
	info.StreamName = session.StreamName()
	info.URLParam = session.RawQuery()
	info.SessionID = session.UniqueKey()
	//info.RemoteAddr = session.GetStat().RemoteAddr
	info.HasInSession = group.HasInSession()
	info.HasOutSession = group.HasOutSession()
	HttpNotify.OnSubStop(info)
}

// ServerObserver of rtsp.Server
func (sm *ServerManager) OnNewRTSPSessionConnect(session *rtsp.ServerCommandSession) {
	// TODO chef: impl me
}

// ServerObserver of rtsp.Server
func (sm *ServerManager) OnDelRTSPSession(session *rtsp.ServerCommandSession) {
	// TODO chef: impl me
}

// ServerObserver of rtsp.Server
func (sm *ServerManager) OnNewRTSPPubSession(session *rtsp.PubSession) bool {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()
	group := sm.getOrCreateGroup("", session.StreamName())
	res := group.AddRTSPPubSession(session)

	var info base.PubStartInfo
	info.ServerID = config.ServerID
	info.Protocol = base.ProtocolRTSP
	info.URL = session.URL()
	info.AppName = session.AppName()
	info.StreamName = session.StreamName()
	info.URLParam = session.RawQuery()
	info.SessionID = session.UniqueKey()
	//info.RemoteAddr = session.GetStat().RemoteAddr
	info.HasInSession = group.HasInSession()
	info.HasOutSession = group.HasOutSession()
	HttpNotify.OnPubStart(info)

	return res
}

// ServerObserver of rtsp.Server
func (sm *ServerManager) OnDelRTSPPubSession(session *rtsp.PubSession) {
	// TODO chef: impl me
	sm.mutex.Lock()
	defer sm.mutex.Unlock()
	group := sm.getGroup("", session.StreamName())
	if group == nil {
		return
	}

	group.DelRTSPPubSession(session)

	var info base.PubStopInfo
	info.ServerID = config.ServerID
	info.Protocol = base.ProtocolRTSP
	info.URL = session.URL()
	info.AppName = session.AppName()
	info.StreamName = session.StreamName()
	info.URLParam = session.RawQuery()
	info.SessionID = session.UniqueKey()
	//info.RemoteAddr = session.GetStat().RemoteAddr
	info.HasInSession = group.HasInSession()
	info.HasOutSession = group.HasOutSession()
	HttpNotify.OnPubStop(info)
}

// ServerObserver of rtsp.Server
func (sm *ServerManager) OnNewRTSPSubSessionDescribe(session *rtsp.SubSession) (ok bool, sdp []byte) {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()
	group := sm.getOrCreateGroup("", session.StreamName())
	return group.HandleNewRTSPSubSessionDescribe(session)
}

// ServerObserver of rtsp.Server
func (sm *ServerManager) OnNewRTSPSubSessionPlay(session *rtsp.SubSession) bool {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()
	group := sm.getOrCreateGroup("", session.StreamName())

	res := group.HandleNewRTSPSubSessionPlay(session)

	var info base.SubStartInfo
	info.ServerID = config.ServerID
	info.Protocol = base.ProtocolRTSP
	info.URL = session.URL()
	info.AppName = session.AppName()
	info.StreamName = session.StreamName()
	info.URLParam = session.RawQuery()
	info.SessionID = session.UniqueKey()
	//info.RemoteAddr = session.GetStat().RemoteAddr
	info.HasInSession = group.HasInSession()
	info.HasOutSession = group.HasOutSession()
	HttpNotify.OnSubStart(info)

	return res
}

// ServerObserver of rtsp.Server
func (sm *ServerManager) OnDelRTSPSubSession(session *rtsp.SubSession) {
	// TODO chef: impl me
	sm.mutex.Lock()
	defer sm.mutex.Unlock()
	group := sm.getGroup("", session.StreamName())
	if group == nil {
		return
	}

	group.DelRTSPSubSession(session)

	var info base.SubStopInfo
	info.ServerID = config.ServerID
	info.Protocol = base.ProtocolRTSP
	info.URL = session.URL()
	info.AppName = session.AppName()
	info.StreamName = session.StreamName()
	info.URLParam = session.RawQuery()
	info.SessionID = session.UniqueKey()
//	info.RemoteAddr = session.GetStat().RemoteAddr
	info.HasInSession = group.HasInSession()
	info.HasOutSession = group.HasOutSession()
	HttpNotify.OnSubStop(info)
}

// HTTPAPIServerObserver
func (sm *ServerManager) OnStatAllGroup() (sgs []base.StatGroup) {
	return sm.statAllGroup()
}

// HTTPAPIServerObserver
func (sm *ServerManager) OnStatGroup(streamName string) *base.StatGroup {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()
	g := sm.getGroup("fakeAppName", streamName)
	if g == nil {
		return nil
	}
	// copy
	var ret base.StatGroup
	ret = g.GetStat()
	return &ret
}

// HTTPAPIServerObserver
func (sm *ServerManager) OnCtrlStartPull(info base.APICtrlStartPullReq) {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()
	g := sm.getGroup(info.AppName, info.StreamName)
	if g == nil {
		nazalog.Warnf("group not exist, ignore start pull. streamName=%s", info.StreamName)
		return
	}
	var url string
	if info.URLParam != "" {
		url = fmt.Sprintf("rtmp://%s/%s/%s?%s", info.Addr, info.AppName, info.StreamName, info.URLParam)
	} else {
		url = fmt.Sprintf("rtmp://%s/%s/%s", info.Addr, info.AppName, info.StreamName)
	}
	g.StartPull(url)
}

// HTTPAPIServerObserver
func (sm *ServerManager) OnCtrlKickOutSession(info base.APICtrlKickOutSession) base.HTTPResponseBasic {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()
	g := sm.getGroup("fake", info.StreamName)
	if g == nil {
		return base.HTTPResponseBasic{
			ErrorCode: base.ErrorCodeGroupNotFound,
			Desp:      base.DespGroupNotFound,
		}
	}
	if !g.KickOutSession(info.SessionID) {
		return base.HTTPResponseBasic{
			ErrorCode: base.ErrorCodeSessionNotFound,
			Desp:      base.DespSessionNotFound,
		}
	}
	return base.HTTPResponseBasic{
		ErrorCode: base.ErrorCodeSucc,
		Desp:      base.DespSucc,
	}
}

func (sm *ServerManager) iterateGroup() {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()
	for k, group := range sm.groupMap {
		// 关闭空闲的group
		if group.IsTotalEmpty() {
			nazalog.Infof("erase empty group. [%s]", group.UniqueKey)
			group.Dispose()
			delete(sm.groupMap, k)
			continue
		}

		group.Tick()
	}
}

func (sm *ServerManager) getOrCreateGroup(appName string, streamName string) *Group {
	group, exist := sm.groupMap[streamName]
	if !exist {
		pullURL := fmt.Sprintf("rtmp://%s/%s/%s", config.RelayPullConfig.Addr, appName, streamName)
		group = NewGroup(appName, streamName, config.RelayPullConfig.Enable, pullURL)
		sm.groupMap[streamName] = group

		go group.RunLoop()
	}
	return group
}

func (sm *ServerManager) getGroup(appName string, streamName string) *Group {
	group, exist := sm.groupMap[streamName]
	if !exist {
		return nil
	}
	return group
}

func (sm *ServerManager) statAllGroup() (sgs []base.StatGroup) {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()
	for _, g := range sm.groupMap {
		sgs = append(sgs, g.GetStat())
	}
	return
}
