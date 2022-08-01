// Copyright 2019, Chef.  All rights reserved.
// https://github.com/q191201771/lal
//
// Use of this source code is governed by a MIT-style license
// that can be found in the License file.
//
// Author: Chef (191201771@qq.com)

package logic

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/q191201771/lal/pkg/mpegts"

	"github.com/q191201771/lal/pkg/remux"

	"github.com/q191201771/naza/pkg/defertaskthread"

	"github.com/q191201771/lal/pkg/rtprtcp"

	"github.com/q191201771/lal/pkg/hevc"

	"github.com/q191201771/lal/pkg/httpts"

	"github.com/q191201771/lal/pkg/base"

	"github.com/q191201771/lal/pkg/avc"
	"github.com/q191201771/lal/pkg/rtsp"

	"github.com/q191201771/lal/pkg/hls"

	"github.com/q191201771/lal/pkg/httpflv"
	"github.com/q191201771/lal/pkg/rtmp"
	"github.com/q191201771/naza/pkg/nazalog"
)

type Group struct {
	UniqueKey  string // const after init
	appName    string // const after init
	streamName string // const after init TODO chef: 和stat里的字段重复，可以删除掉

	exitChan chan struct{}

	mutex sync.Mutex
	//
	stat base.StatGroup
	// pub
	rtmpPubSession *rtmp.ServerSession
	rtspPubSession *rtsp.PubSession
	// pull
	pullEnable bool
	pullURL    string
	pullProxy  *pullProxy
	// sub
	rtmpSubSessionSet    map[*rtmp.ServerSession]struct{}
	httpflvSubSessionSet map[*httpflv.SubSession]struct{}
	httptsSubSessionSet  map[*httpts.SubSession]struct{}
	rtspSubSessionSet    map[*rtsp.SubSession]struct{}
	// push
	url2PushProxy map[string]*pushProxy
	// hls
	hlsMuxer *hls.Muxer

	recordFLV    *httpflv.FLVFileWriter
	recordMPEGTS *mpegts.FileWriter

	// rtmp pub/pull使用
	rtmpGopCache    *GOPCache
	httpflvGopCache *GOPCache

	//
	rtmpBufWriter base.IBufWriter // TODO(chef): 后面可以在业务层加一个定时Flush

	// rtsp pub使用
	asc []byte
	vps []byte
	sps []byte
	pps []byte

	// mpegts使用
	patpmt []byte

	//
	tickCount uint32

	starttime time.Time
	mark bool
	total_rb int64
}

type pullProxy struct {
	isPulling   bool
	pullSession *rtmp.PullSession
}

type pushProxy struct {
	isPushing   bool
	pushSession *rtmp.PushSession
}

func NewGroup(appName string, streamName string, pullEnable bool, pullURL string) *Group {
	uk := base.GenUKGroup()

	url2PushProxy := make(map[string]*pushProxy)
	if config.RelayPushConfig.Enable {
		for _, addr := range config.RelayPushConfig.AddrList {
			url := fmt.Sprintf("rtmp://%s/%s/%s", addr, appName, streamName)
			url2PushProxy[url] = &pushProxy{
				isPushing:   false,
				pushSession: nil,
			}
		}
	}

	g := &Group{
		UniqueKey:  uk,
		appName:    appName,
		streamName: streamName,
		stat: base.StatGroup{
			StreamName: streamName,
		},
		exitChan:             make(chan struct{}, 1),
		rtmpSubSessionSet:    make(map[*rtmp.ServerSession]struct{}),
		httpflvSubSessionSet: make(map[*httpflv.SubSession]struct{}),
		httptsSubSessionSet:  make(map[*httpts.SubSession]struct{}),
		rtspSubSessionSet:    make(map[*rtsp.SubSession]struct{}),
		rtmpGopCache:         NewGOPCache("rtmp", uk, config.RTMPConfig.GOPNum),
		httpflvGopCache:      NewGOPCache("httpflv", uk, config.HTTPFLVConfig.GOPNum),
		pullProxy:            &pullProxy{},
		url2PushProxy:        url2PushProxy,
		pullEnable:           pullEnable,
		pullURL:              pullURL,
		starttime:	time.Now(),	//cx
		mark: false,
	}
	g.rtmpBufWriter = base.NewWriterFuncSize(g.write2RTMPSubSessions, config.RTMPConfig.MergeWriteSize)
	nazalog.Infof("[%s] lifecycle new group. group=%p, appName=%s, streamName=%s", uk, g, appName, streamName)

	return g
}

func (group *Group) RunLoop() {
	<-group.exitChan
}

// TODO chef: 传入时间
// 目前每秒触发一次
func (group *Group) Tick() {
	group.mutex.Lock()
	defer group.mutex.Unlock()

	// 没有sub播放者了关闭pull回源
	group.stopPullIfNeeded()
	// 还有sub播放者，没在pull就触发pull
	group.pullIfNeeded()
	// 还有pub推流，没在push就触发push
	group.pushIfNeeded()

	// TODO chef:
	// 梳理和naza.Connection超时重复部分

	// 定时关闭没有数据的session
	if group.tickCount%checkSessionAliveIntervalSec == 0 {
		if group.rtmpPubSession != nil {
			if readAlive, _ := group.rtmpPubSession.IsAlive(); !readAlive {
				nazalog.Warnf("[%s] session timeout. session=%s", group.UniqueKey, group.rtmpPubSession.UniqueKey())
				group.rtmpPubSession.Dispose()
			}
		}
		if group.rtspPubSession != nil {
			if readAlive, _ := group.rtspPubSession.IsAlive(); !readAlive {
				nazalog.Warnf("[%s] session timeout. session=%s", group.UniqueKey, group.rtspPubSession.UniqueKey())
				group.rtspPubSession.Dispose()
				group.rtspPubSession = nil
			}
		}
		if group.pullProxy.pullSession != nil {
			if readAlive, _ := group.pullProxy.pullSession.IsAlive(); !readAlive {
				nazalog.Warnf("[%s] session timeout. session=%s", group.UniqueKey, group.pullProxy.pullSession.UniqueKey())
				group.pullProxy.pullSession.Dispose()
				group.delRTMPPullSession(group.pullProxy.pullSession)
			}
		}
		for session := range group.rtmpSubSessionSet {
			if _, writeAlive := session.IsAlive(); !writeAlive {
				nazalog.Warnf("[%s] session timeout. session=%s", group.UniqueKey, session.UniqueKey())
				session.Dispose()
				group.delRTMPSubSession(session)
			}
		}
		for session := range group.httpflvSubSessionSet {
			if _, writeAlive := session.IsAlive(); !writeAlive {
				nazalog.Warnf("[%s] session timeout. session=%s", group.UniqueKey, session.UniqueKey())
				session.Dispose()
				group.delHTTPFLVSubSession(session)
			}
		}
		for session := range group.httptsSubSessionSet {
			if _, writeAlive := session.IsAlive(); !writeAlive {
				nazalog.Warnf("[%s] session timeout. session=%s", group.UniqueKey, session.UniqueKey())
				session.Dispose()
				group.delHTTPTSSubSession(session)
			}
		}
		for session := range group.rtspSubSessionSet {
			if _, writeAlive := session.IsAlive(); !writeAlive {
				nazalog.Warnf("[%s] session timeout. session=%s", group.UniqueKey, session.UniqueKey())
				session.Dispose()
				group.delRTSPSubSession(session)
			}
		}
	}

	// 定时计算session bitrate
	if group.tickCount%calcSessionStatIntervalSec == 0 {
		if group.rtmpPubSession != nil {
			group.rtmpPubSession.UpdateStat(calcSessionStatIntervalSec)
		}
		if group.rtspPubSession != nil {
			group.rtspPubSession.UpdateStat(calcSessionStatIntervalSec)
		}
		if group.pullProxy.pullSession != nil {
			group.pullProxy.pullSession.UpdateStat(calcSessionStatIntervalSec)
		}
		for session := range group.rtmpSubSessionSet {
			session.UpdateStat(calcSessionStatIntervalSec)
		}
		for session := range group.httpflvSubSessionSet {
			session.UpdateStat(calcSessionStatIntervalSec)
		}
		for session := range group.httptsSubSessionSet {
			session.UpdateStat(calcSessionStatIntervalSec)
		}
		for session := range group.rtspSubSessionSet {
			session.UpdateStat(calcSessionStatIntervalSec)
		}
	}
	group.tickCount++
}

// 主动释放所有资源。保证所有资源的生命周期逻辑上都在我们的控制中。降低出bug的几率，降低心智负担。
// 注意，Dispose后，不应再使用这个对象。
// 值得一提，如果是从其他协程回调回来的消息，在使用Group中的资源前，要判断资源是否存在以及可用。
//
// TODO chef:
//  后续弄个协程来替换掉目前锁的方式，来做消息同步。这样有个好处，就是不用写很多的资源有效判断。统一写一个就好了。
//  目前Dispose在IsTotalEmpty时调用，暂时没有这个问题。
func (group *Group) Dispose() {
	nazalog.Infof("[%s] lifecycle dispose group.", group.UniqueKey)
	group.exitChan <- struct{}{}

	group.mutex.Lock()
	defer group.mutex.Unlock()

	if group.rtmpPubSession != nil {
		group.rtmpPubSession.Dispose()
		group.rtmpPubSession = nil
	}
	if group.rtspPubSession != nil {
		group.rtspPubSession.Dispose()
		group.rtspPubSession = nil
	}

	for session := range group.rtmpSubSessionSet {
		session.Dispose()
	}
	group.rtmpSubSessionSet = nil

	for session := range group.httpflvSubSessionSet {
		session.Dispose()
	}
	group.httpflvSubSessionSet = nil

	for session := range group.httptsSubSessionSet {
		session.Dispose()
	}
	group.httptsSubSessionSet = nil

	group.disposeHLSMuxer()

	if config.RelayPushConfig.Enable {
		for _, v := range group.url2PushProxy {
			if v.pushSession != nil {
				v.pushSession.Dispose()
			}
		}
		group.url2PushProxy = nil
	}
}

func (group *Group) AddRTMPPubSession(session *rtmp.ServerSession) bool {
	nazalog.Debugf("[%s] [%s] add PubSession into group.", group.UniqueKey, session.UniqueKey())

	group.mutex.Lock()
	defer group.mutex.Unlock()

	if group.hasInSession() {
		nazalog.Errorf("[%s] in stream already exist. wanna add=%s", group.UniqueKey, session.UniqueKey())
		return false
	}

	group.rtmpPubSession = session
	group.addIn()
	session.SetPubSessionObserver(group)

	return true
}

func (group *Group) DelRTMPPubSession(session *rtmp.ServerSession) {
	group.mutex.Lock()
	defer group.mutex.Unlock()
	group.delRTMPPubSession(session)
}

// TODO chef: rtsp package中，增加回调返回值判断，如果是false，将连接关掉
func (group *Group) AddRTSPPubSession(session *rtsp.PubSession) bool {
	nazalog.Debugf("[%s] [%s] add RTSP PubSession into group.", group.UniqueKey, session.UniqueKey())

	group.mutex.Lock()
	defer group.mutex.Unlock()

	if group.hasInSession() {
		nazalog.Errorf("[%s] in stream already exist. wanna add=%s", group.UniqueKey, session.UniqueKey())
		return false
	}

	group.rtspPubSession = session
	group.addIn()
	session.SetObserver(group)

	return true
}

func (group *Group) DelRTSPPubSession(session *rtsp.PubSession) {
	group.mutex.Lock()
	defer group.mutex.Unlock()
	group.delRTSPPubSession(session)
}

func (group *Group) AddRTMPPullSession(session *rtmp.PullSession) bool {
	nazalog.Debugf("[%s] [%s] add PullSession into group.", group.UniqueKey, session.UniqueKey())

	group.mutex.Lock()
	defer group.mutex.Unlock()

	if group.hasInSession() {
		nazalog.Errorf("[%s] in stream already exist. wanna add=%s", group.UniqueKey, session.UniqueKey())
		return false
	}

	group.pullProxy.pullSession = session
	group.addIn()
	return true
}

func (group *Group) DelRTMPPullSession(session *rtmp.PullSession) {
	group.mutex.Lock()
	defer group.mutex.Unlock()
	group.delRTMPPullSession(session)
}

func (group *Group) AddRTMPSubSession(session *rtmp.ServerSession) {
	nazalog.Debugf("[%s] [%s] add SubSession into group.", group.UniqueKey, session.UniqueKey())
	group.mutex.Lock()
	defer group.mutex.Unlock()
	group.rtmpSubSessionSet[session] = struct{}{}

	group.pullIfNeeded()
}

func (group *Group) DelRTMPSubSession(session *rtmp.ServerSession) {
	group.mutex.Lock()
	defer group.mutex.Unlock()
	group.delRTMPSubSession(session)
}

func (group *Group) AddHTTPFLVSubSession(session *httpflv.SubSession) {
	nazalog.Debugf("[%s] [%s] add httpflv SubSession into group.", group.UniqueKey, session.UniqueKey())
	session.WriteHTTPResponseHeader()
	session.WriteFLVHeader()

	group.mutex.Lock()
	defer group.mutex.Unlock()
	group.httpflvSubSessionSet[session] = struct{}{}

	group.pullIfNeeded()
}

func (group *Group) DelHTTPFLVSubSession(session *httpflv.SubSession) {
	group.mutex.Lock()
	defer group.mutex.Unlock()
	group.delHTTPFLVSubSession(session)
}

// TODO chef:
//   这里应该也要考虑触发hls muxer开启
//   也即HTTPTS sub需要使用hls muxer，hls muxer开启和关闭都要考虑HTTPTS sub
func (group *Group) AddHTTPTSSubSession(session *httpts.SubSession) {
	nazalog.Debugf("[%s] [%s] add httpts SubSession into group.", group.UniqueKey, session.UniqueKey())
	session.WriteHTTPResponseHeader()

	group.mutex.Lock()
	defer group.mutex.Unlock()
	group.httptsSubSessionSet[session] = struct{}{}

	group.pullIfNeeded()
}

func (group *Group) DelHTTPTSSubSession(session *httpts.SubSession) {
	group.mutex.Lock()
	defer group.mutex.Unlock()
	group.delHTTPTSSubSession(session)
}

func (group *Group) HandleNewRTSPSubSessionDescribe(session *rtsp.SubSession) (ok bool, sdp []byte) {
	group.mutex.Lock()
	defer group.mutex.Unlock()
	if group.rtspPubSession == nil {
		nazalog.Warnf("[%s] close rtsp subSession while describe but pubSession not exist. [%s]",
			group.UniqueKey, session.UniqueKey())
		return false, nil
	}

	sdp, _ = group.rtspPubSession.GetSDP()
	return true, sdp
}

func (group *Group) HandleNewRTSPSubSessionPlay(session *rtsp.SubSession) bool {
	nazalog.Debugf("[%s] [%s] add rtsp SubSession into group.", group.UniqueKey, session.UniqueKey())

	group.mutex.Lock()
	defer group.mutex.Unlock()
	group.rtspSubSessionSet[session] = struct{}{}
	return true
}

func (group *Group) DelRTSPSubSession(session *rtsp.SubSession) {
	nazalog.Debugf("[%s] [%s] del rtsp SubSession from group.", group.UniqueKey, session.UniqueKey())
	group.mutex.Lock()
	defer group.mutex.Unlock()
	delete(group.rtspSubSessionSet, session)
}

func (group *Group) AddRTMPPushSession(url string, session *rtmp.PushSession) {
	nazalog.Debugf("[%s] [%s] add rtmp PushSession into group.", group.UniqueKey, session.UniqueKey())
	group.mutex.Lock()
	defer group.mutex.Unlock()
	if group.url2PushProxy != nil {
		group.url2PushProxy[url].pushSession = session
	}
}

func (group *Group) DelRTMPPushSession(url string, session *rtmp.PushSession) {
	nazalog.Debugf("[%s] [%s] del rtmp PushSession into group.", group.UniqueKey, session.UniqueKey())
	group.mutex.Lock()
	defer group.mutex.Unlock()
	if group.url2PushProxy != nil {
		group.url2PushProxy[url].pushSession = nil
		group.url2PushProxy[url].isPushing = false
	}
}

func (group *Group) IsTotalEmpty() bool {
	group.mutex.Lock()
	defer group.mutex.Unlock()
	return group.isTotalEmpty()
}

func (group *Group) HasInSession() bool {
	group.mutex.Lock()
	defer group.mutex.Unlock()
	return group.hasInSession()
}

func (group *Group) HasOutSession() bool {
	group.mutex.Lock()
	defer group.mutex.Unlock()
	return group.hasOutSession()
}

func (group *Group) BroadcastByRTMPMsg(msg base.RTMPMsg) {
	group.mutex.Lock()
	defer group.mutex.Unlock()
	group.broadcastByRTMPMsg(msg)
}

// hls.Muxer
func (group *Group) OnPATPMT(b []byte) {
	group.patpmt = b

	if group.recordMPEGTS != nil {
		if err := group.recordMPEGTS.Write(b); err != nil {
			nazalog.Errorf("[%s] record mpegts write fragment header error. err=%+v", group.UniqueKey, err)
		}
	}
}

// hls.Muxer
func (group *Group) OnTSPackets(rawFrame []byte, boundary bool) {
	// 因为最前面Feed时已经加锁了，所以这里回调上来就不用加锁了

	for session := range group.httptsSubSessionSet {
		if session.IsFresh {
			if boundary {
				session.Write(group.patpmt)
				session.Write(rawFrame)
				session.IsFresh = false
			}
		} else {
			session.Write(rawFrame)
		}
	}

	if group.recordMPEGTS != nil {
		if err := group.recordMPEGTS.Write(rawFrame); err != nil {
			nazalog.Errorf("[%s] record mpegts write error. err=%+v", group.UniqueKey, err)
		}
	}
}

// rtmp.PubSession or rtmp.PullSession
func (group *Group) OnReadRTMPAVMsg(msg base.RTMPMsg) {
	group.BroadcastByRTMPMsg(msg)
}

// rtsp.PubSession
func (group *Group) OnRTPPacket(pkt rtprtcp.RTPPacket) {
	group.mutex.Lock()
	defer group.mutex.Unlock()

	for s := range group.rtspSubSessionSet {
		s.WriteRTPPacket(pkt)
	}
}

// rtsp.PubSession
func (group *Group) OnAVConfig(asc, vps, sps, pps []byte) {
	// 注意，前面已经进锁了，这里依然在锁保护内

	group.asc = asc
	group.vps = vps
	group.sps = sps
	group.pps = pps

	metadata, vsh, ash, err := remux.AVConfig2RTMPMsg(group.asc, group.vps, group.sps, group.pps)
	if err != nil {
		nazalog.Errorf("[%s] remux avconfig to metadata and seqheader failed. err=%+v", group.UniqueKey, err)
		return
	}
	if metadata != nil {
		group.broadcastByRTMPMsg(*metadata)
	}
	if vsh != nil {
		group.broadcastByRTMPMsg(*vsh)
	}
	if ash != nil {
		group.broadcastByRTMPMsg(*ash)
	}
}

// rtsp.PubSession
func (group *Group) OnAVPacket(pkt base.AVPacket) {
	//nazalog.Tracef("[%s] > Group::OnAVPacket. type=%s, ts=%d", group.UniqueKey, pkt.PayloadType.ReadableString(), pkt.Timestamp)
	msg, err := remux.AVPacket2RTMPMsg(pkt)
	if err != nil {
		nazalog.Errorf("[%s] remux av packet to rtmp msg failed. err=+%v", group.UniqueKey, err)
		return
	}

	group.BroadcastByRTMPMsg(msg)
}

func (group *Group) StringifyDebugStats() string {
	group.mutex.Lock()
	subLen := len(group.rtmpSubSessionSet) + len(group.httpflvSubSessionSet) + len(group.httptsSubSessionSet) + len(group.rtspSubSessionSet)
	group.mutex.Unlock()
	if subLen > 10 {
		return fmt.Sprintf("[%s] not log out all stats. subLen=%d", group.UniqueKey, subLen)
	}
	b, _ := json.Marshal(group.GetStat())
	return string(b)
}

func (group *Group) GetStat() base.StatGroup {
	group.mutex.Lock()
	defer group.mutex.Unlock()

	if group.rtmpPubSession != nil {
		group.stat.StatPub = base.StatSession2Pub(group.rtmpPubSession.GetStat())
	} else if group.rtspPubSession != nil {
		group.stat.StatPub = base.StatSession2Pub(group.rtspPubSession.GetStat())
	} else {
		group.stat.StatPub = base.StatPub{}
	}

	group.stat.StatSubs = nil
	for s := range group.rtmpSubSessionSet {
		group.stat.StatSubs = append(group.stat.StatSubs, base.StatSession2Sub(s.GetStat()))
	}
	for s := range group.httpflvSubSessionSet {
		group.stat.StatSubs = append(group.stat.StatSubs, base.StatSession2Sub(s.GetStat()))
	}
	for s := range group.httptsSubSessionSet {
		group.stat.StatSubs = append(group.stat.StatSubs, base.StatSession2Sub(s.GetStat()))
	}
	for s := range group.rtspSubSessionSet {
		group.stat.StatSubs = append(group.stat.StatSubs, base.StatSession2Sub(s.GetStat()))
	}

	if group.pullProxy.pullSession != nil {
		group.stat.StatPull = base.StatSession2Pull(group.pullProxy.pullSession.GetStat())
	}

	return group.stat
}

func (group *Group) StartPull(url string) {
	group.mutex.Lock()
	defer group.mutex.Unlock()

	group.pullEnable = true
	group.pullURL = url
	group.pullIfNeeded()
}

func (group *Group) IsHLSMuxerAlive() bool {
	group.mutex.Lock()
	defer group.mutex.Unlock()
	return group.hlsMuxer != nil
}

func (group *Group) KickOutSession(sessionID string) bool {
	group.mutex.Lock()
	defer group.mutex.Unlock()

	nazalog.Infof("[%s] kick out session. session id=%s", group.UniqueKey, sessionID)

	if strings.HasPrefix(sessionID, base.UKPreRTMPServerSession) {
		if group.rtmpPubSession != nil {
			group.rtmpPubSession.Dispose()
			return true
		}
	} else if strings.HasPrefix(sessionID, base.UKPreRTSPPubSession) {
		if group.rtspPubSession != nil {
			group.rtspPubSession.Dispose()
			return true
		}
	} else if strings.HasPrefix(sessionID, base.UKPreFLVSubSession) {
		// TODO chef: 考虑数据结构改成sessionIDzuokey的map
		for s := range group.httpflvSubSessionSet {
			if s.UniqueKey() == sessionID {
				s.Dispose()
				return true
			}
		}
	} else if strings.HasPrefix(sessionID, base.UKPreTSSubSession) {
		for s := range group.httptsSubSessionSet {
			if s.UniqueKey() == sessionID {
				s.Dispose()
				return true
			}
		}
	} else if strings.HasPrefix(sessionID, base.UKPreRTSPSubSession) {
		// TODO chef: impl me
	} else {
		nazalog.Errorf("[%s] kick out session while session id format invalid. %s", group.UniqueKey, sessionID)
	}

	return false
}

func (group *Group) delRTMPPubSession(session *rtmp.ServerSession) {
	nazalog.Debugf("[%s] [%s] del rtmp PubSession from group.", group.UniqueKey, session.UniqueKey())

	if session != group.rtmpPubSession {
		nazalog.Warnf("[%s] del rtmp pub session but not match. del session=%s, group session=%p", group.UniqueKey, session.UniqueKey(), group.rtmpPubSession)
		return
	}

	group.rtmpPubSession = nil
	group.delIn()
}

func (group *Group) delRTSPPubSession(session *rtsp.PubSession) {
	nazalog.Debugf("[%s] [%s] del rtsp PubSession from group.", group.UniqueKey, session.UniqueKey())

	if session != group.rtspPubSession {
		nazalog.Warnf("[%s] del rtmp pub session but not match. del session=%s, group session=%p", group.UniqueKey, session.UniqueKey(), group.rtmpPubSession)
		return
	}

	_ = group.rtspPubSession.Dispose()
	group.rtspPubSession = nil
	group.delIn()
}

func (group *Group) delRTMPPullSession(session *rtmp.PullSession) {
	nazalog.Debugf("[%s] [%s] del rtmp PullSession from group.", group.UniqueKey, session.UniqueKey())

	group.pullProxy.pullSession = nil
	group.pullProxy.isPulling = false
	group.delIn()
}

func (group *Group) delRTMPSubSession(session *rtmp.ServerSession) {
	nazalog.Debugf("[%s] [%s] del rtmp SubSession from group.", group.UniqueKey, session.UniqueKey())
	delete(group.rtmpSubSessionSet, session)
}

func (group *Group) delHTTPFLVSubSession(session *httpflv.SubSession) {
	nazalog.Debugf("[%s] [%s] del httpflv SubSession from group.", group.UniqueKey, session.UniqueKey())
	delete(group.httpflvSubSessionSet, session)
}

func (group *Group) delHTTPTSSubSession(session *httpts.SubSession) {
	nazalog.Debugf("[%s] [%s] del httpts SubSession from group.", group.UniqueKey, session.UniqueKey())
	delete(group.httptsSubSessionSet, session)
}

func (group *Group) delRTSPSubSession(session *rtsp.SubSession) {
	nazalog.Debugf("[%s] [%s] del rtsp SubSession from group.", group.UniqueKey, session.UniqueKey())
	delete(group.rtspSubSessionSet, session)
}

// TODO chef: 目前相当于其他类型往rtmp.AVMsg转了，考虑统一往一个通用类型转
// @param msg 调用结束后，内部不持有msg.Payload内存块
func (group *Group) broadcastByRTMPMsg(msg base.RTMPMsg) {
	var (
		lcd    LazyChunkDivider
		lrm2ft LazyRTMPMsg2FLVTag
		
		// now		time.Time			//cx add
		// dur 	int64
		// gap		int64
	)

	//nazalog.Debugf("[%s] broadcaseRTMP. header=%+v, %s", group.UniqueKey, msg.Header, hex.Dump(nazastring.SubSliceSafety(msg.Payload, 7)))

	// # 0. hls
	if config.HLSConfig.Enable && group.hlsMuxer != nil {
		group.hlsMuxer.FeedRTMPMessage(msg)
	}

	// # 1. 设置好用于发送的 rtmp 头部信息
	currHeader := remux.MakeDefaultRTMPHeader(msg.Header)
	if currHeader.MsgLen != uint32(len(msg.Payload)) {
		nazalog.Errorf("[%s] diff. msgLen=%d, payload len=%d, %+v", group.UniqueKey, currHeader.MsgLen, len(msg.Payload), msg.Header)
	}

	// # 2. 懒初始化rtmp chunk切片，以及httpflv转换
	lcd.Init(msg.Payload, &currHeader)
	lrm2ft.Init(msg)

	// # 3. 广播。遍历所有 rtmp sub session，转发数据
	// ## 3.1. 如果是新的 sub session，发送已缓存的信息
	for session := range group.rtmpSubSessionSet {
		
		if session.IsFresh {
			// TODO chef: 头信息和full gop也可以在SubSession刚加入时发送
			if group.rtmpGopCache.Metadata != nil {
				//nazalog.Debugf("[%s] [%s] write metadata", group.UniqueKey, session.UniqueKey)
				_ = session.Write(group.rtmpGopCache.Metadata)
			}
			if group.rtmpGopCache.VideoSeqHeader != nil {
				//nazalog.Debugf("[%s] [%s] write vsh", group.UniqueKey, session.UniqueKey)
				_ = session.Write(group.rtmpGopCache.VideoSeqHeader)
			}
			if group.rtmpGopCache.AACSeqHeader != nil {
				//nazalog.Debugf("[%s] [%s] write ash", group.UniqueKey, session.UniqueKey)
				_ = session.Write(group.rtmpGopCache.AACSeqHeader)
			}
			for i := 0; i < group.rtmpGopCache.GetGOPCount(); i++ {
				for _, item := range group.rtmpGopCache.GetGOPDataAt(i) {
					_ = session.Write(item)
				}
			}

			session.IsFresh = false
		}
	}
	// ## 3.2. 转发本次数据
	if len(group.rtmpSubSessionSet) > 0 {
		//nazalog.Debugf("herehere")
		group.rtmpBufWriter.Write(lcd.Get())
	}

	// TODO chef: rtmp sub, rtmp push, httpflv sub 的发送逻辑都差不多，可以考虑封装一下
	if config.RelayPushConfig.Enable {
		for _, v := range group.url2PushProxy {
			if v.pushSession == nil {
				continue
			}

			if v.pushSession.IsFresh {
				
				if group.rtmpGopCache.Metadata != nil {
					_ = v.pushSession.Write(group.rtmpGopCache.Metadata)
				}
				if group.rtmpGopCache.VideoSeqHeader != nil {
					_ = v.pushSession.Write(group.rtmpGopCache.VideoSeqHeader)
				}
				if group.rtmpGopCache.AACSeqHeader != nil {
					_ = v.pushSession.Write(group.rtmpGopCache.AACSeqHeader)
				}
				for i := 0; i < group.rtmpGopCache.GetGOPCount(); i++ {
					for _, item := range group.rtmpGopCache.GetGOPDataAt(i) {
						_ = v.pushSession.Write(item)
					}
				}

				v.pushSession.IsFresh = false
			}

			_ = v.pushSession.Write(lcd.Get())
		}
	}
	// cx add for debug 

	//nazalog.Debugf("msg : %v",msg.Header)
	if(group.mark == false){

		group.starttime = time.Now()
		group.mark = true
		start_tick := time.Now()
		go func() {
			var rbing bool
			var start_rb, end_rb  time.Time
			rbing = false
			for{
				if(group.rtmpGopCache.isGOPRingEmpty() && rbing == false){

					start_rb = time.Now()
					rbing = true
				}else if(group.rtmpGopCache.isGOPRingEmpty() && rbing == true){
					//nazalog.Debugf("cond 2")
					continue
				}else if(!group.rtmpGopCache.isGOPRingEmpty() && rbing == true){
					end_rb = time.Now()
					group.total_rb += end_rb.Sub(start_rb).Milliseconds()
					rbing = false
					//nazalog.Debugf("rb stalling :%v ", group.total_rb)
				}else{
					// almost have data in gopring
					//nazalog.Debugf("almost no rb")
					continue
				}
			}
		}()
		nazalog.Debugf("Livestreaming start receiving tick:%v,", start_tick.UnixNano()/ 1000)
	}
	// now = time.Now()
	// dur = now.Sub(group.starttime).Milliseconds()			// passtime - 0 
	
	// gap = (int64)(msg.Header.TimestampAbs) -  dur
	// nazalog.Debugf("gap:%d:",gap)		// if >0 ok ; <0 overdue

	// # 4. 广播。遍历所有 httpflv sub session，转发数据
	for session := range group.httpflvSubSessionSet {
		
		if session.IsFresh {
			if group.httpflvGopCache.Metadata != nil {
				session.Write(group.httpflvGopCache.Metadata)
			}
			if group.httpflvGopCache.VideoSeqHeader != nil {
				session.Write(group.httpflvGopCache.VideoSeqHeader)
			}
			if group.httpflvGopCache.AACSeqHeader != nil {
				session.Write(group.httpflvGopCache.AACSeqHeader)
			}
			for i := 0; i < group.httpflvGopCache.GetGOPCount(); i++ {
				for _, item := range group.httpflvGopCache.GetGOPDataAt(i) {
					session.Write(item)
				}
			}

			session.IsFresh = false
		}

		session.Write(lrm2ft.Get())
	}

	// # 5. 录制flv文件
	if group.recordFLV != nil {
		if err := group.recordFLV.WriteRaw(lrm2ft.Get()); err != nil {
			nazalog.Errorf("[%s] record flv write error. err=%+v", group.UniqueKey, err)
		}
	}

	// # 6. 缓存关键信息，以及gop
	if config.RTMPConfig.Enable {
		group.rtmpGopCache.Feed(msg, lcd.Get)
	}
	if config.HTTPFLVConfig.Enable {
		group.httpflvGopCache.Feed(msg, lrm2ft.Get)
	}

	// # 7. 记录stat
	if group.stat.AudioCodec == "" {
		if msg.IsAACSeqHeader() {
			group.stat.AudioCodec = base.AudioCodecAAC
		}
	}
	if group.stat.AudioCodec == "" {
		if msg.IsAVCKeySeqHeader() {
			group.stat.VideoCodec = base.VideoCodecAVC
		}
		if msg.IsHEVCKeySeqHeader() {
			group.stat.VideoCodec = base.VideoCodecHEVC
		}
	}
	if group.stat.VideoHeight == 0 || group.stat.VideoWidth == 0 {
		if msg.IsAVCKeySeqHeader() {
			sps, _, err := avc.ParseSPSPPSFromSeqHeader(msg.Payload)
			if err == nil {
				var ctx avc.Context
				err = avc.ParseSPS(sps, &ctx)
				if err == nil {
					group.stat.VideoHeight = int(ctx.Height)
					group.stat.VideoWidth = int(ctx.Width)
				}
			}
		}
		if msg.IsHEVCKeySeqHeader() {
			_, sps, _, err := hevc.ParseVPSSPSPPSFromSeqHeader(msg.Payload)
			if err == nil {
				var ctx hevc.Context
				err = hevc.ParseSPS(sps, &ctx)
				if err == nil {
					group.stat.VideoHeight = int(ctx.PicHeightInLumaSamples)
					group.stat.VideoWidth = int(ctx.PicWidthInLumaSamples)
				}
			}
		}
	}
}

func (group *Group) write2RTMPSubSessions(b []byte) {
	for session := range group.rtmpSubSessionSet {
		_ = session.Write(b)
	}
}

func (group *Group) stopPullIfNeeded() {
	if group.pullProxy.pullSession != nil && !group.hasOutSession() {
		nazalog.Infof("[%s] stop pull since no sub session.", group.UniqueKey)
		group.pullProxy.pullSession.Dispose()
	}
}

func (group *Group) pullIfNeeded() {
	if !group.pullEnable {
		return
	}
	if !group.hasOutSession() {
		return
	}
	if group.hasInSession() {
		return
	}
	// 正在回源中
	if group.pullProxy.isPulling {
		return
	}
	group.pullProxy.isPulling = true

	nazalog.Infof("[%s] start relay pull. url=%s", group.UniqueKey, group.pullURL)

	go func() {
		pullSession := rtmp.NewPullSession(func(option *rtmp.PullSessionOption) {
			option.PullTimeoutMS = relayPullTimeoutMS
			option.ReadAVTimeoutMS = relayPullReadAVTimeoutMS
		})
		err := pullSession.Pull(group.pullURL, group.OnReadRTMPAVMsg)
		if err != nil {
			nazalog.Errorf("[%s] relay pull fail. err=%v", pullSession.UniqueKey(), err)
			group.DelRTMPPullSession(pullSession)
			return
		}
		res := group.AddRTMPPullSession(pullSession)
		if res {
			err = <-pullSession.WaitChan()
			nazalog.Infof("[%s] relay pull done. err=%v", pullSession.UniqueKey(), err)
			group.DelRTMPPullSession(pullSession)
		} else {
			pullSession.Dispose()
		}
	}()
}

func (group *Group) pushIfNeeded() {
	// push转推功能没开
	if !config.RelayPushConfig.Enable {
		return
	}
	// 没有pub发布者
	if group.rtmpPubSession == nil && group.rtspPubSession == nil {
		return
	}

	// relay push时携带rtmp pub的参数
	// TODO chef: 这个逻辑放这里不太好看
	var urlParam string
	if group.rtmpPubSession != nil {
		urlParam = group.rtmpPubSession.RawQuery()
	}

	for url, v := range group.url2PushProxy {
		// 正在转推中
		if v.isPushing {
			continue
		}
		v.isPushing = true

		urlWithParam := url
		if urlParam != "" {
			urlWithParam += "?" + urlParam
		}
		nazalog.Infof("[%s] start relay push. url=%s", group.UniqueKey, urlWithParam)

		go func(u, u2 string) {
			pushSession := rtmp.NewPushSession("quic",func(option *rtmp.PushSessionOption) {
				option.PushTimeoutMS = relayPushTimeoutMS
				option.WriteAVTimeoutMS = relayPushWriteAVTimeoutMS
			})
			err := pushSession.Push(u2)
			if err != nil {
				nazalog.Errorf("[%s] relay push done. err=%v", pushSession.UniqueKey(), err)
				group.DelRTMPPushSession(u, pushSession)
				return
			}
			group.AddRTMPPushSession(u, pushSession)
			err = <-pushSession.WaitChan()
			nazalog.Infof("[%s] relay push done. err=%v", pushSession.UniqueKey(), err)
			group.DelRTMPPushSession(u, pushSession)
		}(url, urlWithParam)
	}
}

func (group *Group) hasPushSession() bool {
	for _, item := range group.url2PushProxy {
		if item.isPushing || item.pushSession != nil {
			return true
		}
	}
	return false
}

func (group *Group) isTotalEmpty() bool {
	return group.rtmpPubSession == nil &&
		len(group.rtmpSubSessionSet) == 0 &&
		group.rtspPubSession == nil &&
		len(group.httpflvSubSessionSet) == 0 &&
		len(group.httptsSubSessionSet) == 0 &&
		len(group.rtspSubSessionSet) == 0 &&
		group.hlsMuxer == nil &&
		!group.hasPushSession() &&
		group.pullProxy.pullSession == nil
}

func (group *Group) hasInSession() bool {
	return group.rtmpPubSession != nil ||
		group.rtspPubSession != nil ||
		group.pullProxy.pullSession != nil
}

func (group *Group) hasOutSession() bool {
	return len(group.rtmpSubSessionSet) != 0 ||
		len(group.httpflvSubSessionSet) != 0 ||
		len(group.httptsSubSessionSet) != 0 ||
		len(group.rtspSubSessionSet) != 0
}

func (group *Group) addIn() {
	if config.HLSConfig.Enable {
		if group.hlsMuxer != nil {
			nazalog.Errorf("[%s] hls muxer exist while addIn. muxer=%+v", group.UniqueKey, group.hlsMuxer)
		}
		enable := config.HLSConfig.Enable || config.HLSConfig.EnableHTTPS
		group.hlsMuxer = hls.NewMuxer(group.streamName, enable, &config.HLSConfig.MuxerConfig, group)
		group.hlsMuxer.Start()
	}

	if config.RelayPushConfig.Enable {
		group.pushIfNeeded()
	}

	now := time.Now().Unix()
	if config.RecordConfig.EnableFLV {
		filename := fmt.Sprintf("%s%d.flv", group.streamName, now)
		filenameWithPath := filepath.Join(config.RecordConfig.FLVOutPath, filename)
		if group.recordFLV != nil {
			nazalog.Errorf("[%s] record flv but already exist. new filename=%s, old filename=%s",
				group.UniqueKey, filenameWithPath, group.recordFLV.Name())
			if err := group.recordFLV.Dispose(); err != nil {
				nazalog.Errorf("[%s] record flv dispose error. err=%+v", group.UniqueKey, err)
			}
		}
		group.recordFLV = &httpflv.FLVFileWriter{}
		if err := group.recordFLV.Open(filenameWithPath); err != nil {
			nazalog.Errorf("[%s] record flv open file failed. filename=%s, err=%+v",
				group.UniqueKey, filenameWithPath, err)
			group.recordFLV = nil
		}
		if err := group.recordFLV.WriteFLVHeader(); err != nil {
			nazalog.Errorf("[%s] record flv write flv header failed. filename=%s, err=%+v",
				group.UniqueKey, filenameWithPath, err)
			group.recordFLV = nil
		}
	}

	if config.RecordConfig.EnableMPEGTS {
		filename := fmt.Sprintf("%s-%d.ts", group.streamName, now)
		filenameWithPath := filepath.Join(config.RecordConfig.MPEGTSOutPath, filename)
		if group.recordMPEGTS != nil {
			nazalog.Errorf("[%s] record mpegts but already exist. new filename=%s, old filename=%s",
				group.UniqueKey, filenameWithPath, group.recordMPEGTS.Name())
			if err := group.recordMPEGTS.Dispose(); err != nil {
				nazalog.Errorf("[%s] record mpegts dispose error. err=%+v", group.UniqueKey, err)
			}
		}
		group.recordMPEGTS = &mpegts.FileWriter{}
		if err := group.recordMPEGTS.Create(filenameWithPath); err != nil {
			nazalog.Errorf("[%s] record mpegts open file failed. filename=%s, err=%+v",
				group.UniqueKey, filenameWithPath, err)
			group.recordFLV = nil
		}
	}
}

func (group *Group) delIn() {
	if config.HLSConfig.Enable && group.hlsMuxer != nil {
		group.disposeHLSMuxer()
	}

	if config.RelayPushConfig.Enable {
		for _, v := range group.url2PushProxy {
			if v.pushSession != nil {
				v.pushSession.Dispose()
			}
			v.pushSession = nil
		}
	}

	if config.RecordConfig.EnableFLV {
		if group.recordFLV != nil {
			if err := group.recordFLV.Dispose(); err != nil {
				nazalog.Errorf("[%s] record flv dispose error. err=%+v", group.UniqueKey, err)
			}
			group.recordFLV = nil
		}
	}

	if config.RecordConfig.EnableMPEGTS {
		if group.recordMPEGTS != nil {
			if err := group.recordMPEGTS.Dispose(); err != nil {
				nazalog.Errorf("[%s] record mpegts dispose error. err=%+v", group.UniqueKey, err)
			}
			group.recordMPEGTS = nil
		}
	}

	group.rtmpGopCache.Clear()
	group.httpflvGopCache.Clear()

	// TODO(chef) 情况rtsp pub缓存的asc sps pps等数据

	group.patpmt = nil
}

func (group *Group) disposeHLSMuxer() {
	if group.hlsMuxer != nil {
		group.hlsMuxer.Dispose()

		// 添加延时任务，删除HLS文件
		if config.HLSConfig.Enable &&
			(config.HLSConfig.CleanupMode == hls.CleanupModeInTheEnd || config.HLSConfig.CleanupMode == hls.CleanupModeASAP) {
			defertaskthread.Go(
				config.HLSConfig.FragmentDurationMS*config.HLSConfig.FragmentNum*2,
				func(param ...interface{}) {
					appName := param[0].(string)
					streamName := param[1].(string)
					outPath := param[2].(string)

					if g := sm.GetGroup(appName, streamName); g != nil {
						if g.IsHLSMuxerAlive() {
							nazalog.Warnf("cancel cleanup hls file path since hls muxer still alive. streamName=%s", streamName)
							return
						}
					}

					nazalog.Infof("cleanup hls file path. streamName=%s, path=%s", streamName, outPath)
					if err := hls.RemoveAll(outPath); err != nil {
						nazalog.Warnf("cleanup hls file path error. path=%s, err=%+v", outPath, err)
					}
				},
				group.appName,
				group.streamName,
				group.hlsMuxer.OutPath(),
			)
		}

		group.hlsMuxer = nil
	}
}

// TODO chef: 后续看是否有更合适的方法判断
func (group *Group) isHEVC() bool {
	return group.vps != nil
}
