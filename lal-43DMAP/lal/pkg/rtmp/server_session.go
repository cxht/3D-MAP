// Copyright 2019, Chef.  All rights reserved.
// https://github.com/q191201771/lal
//
// Use of this source code is governed by a MIT-style license
// that can be found in the License file.
//
// Author: Chef (191201771@qq.com)

package rtmp

import (
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/q191201771/lal/pkg/base"

	"github.com/q191201771/naza/pkg/bele"
	"github.com/q191201771/naza/pkg/connection"
	"github.com/q191201771/naza/pkg/nazalog"
	//quic "github.com/lucas-clemente/quic-go"
)

// TODO chef: 没有进化成Pub Sub时的超时释放
func initLog(filename string ) {
	_ = nazalog.Init(func(option *nazalog.Option) {
		option.AssertBehavior = nazalog.AssertFatal
		option.Filename = filename
	})
}

type ServerSessionObserver interface {
	OnRTMPConnect(session *ServerSession, opa ObjectPairArray)
	OnNewRTMPPubSession(session *ServerSession) // 上层代码应该在这个事件回调中注册音视频数据的监听
	OnNewRTMPSubSession(session *ServerSession)
}

type PubSessionObserver interface {
	// 注意，回调结束后，内部会复用Payload内存块
	OnReadRTMPAVMsg(msg base.RTMPMsg)
}

func (s *ServerSession) SetPubSessionObserver(observer PubSessionObserver) {
	s.avObserver = observer
}

type ServerSessionType int

const (
	ServerSessionTypeUnknown ServerSessionType = iota // 收到客户端的publish或者play信令之前的类型状态
	ServerSessionTypePub
	ServerSessionTypeSub
)

type ServerSession struct {
	uniqueKey              string // const after ctor
	url                    string
	tcURL                  string
	streamNameWithRawQuery string // const after set
	appName                string // const after set
	streamName             string // const after set
	rawQuery               string //const after set

	observer      ServerSessionObserver
	t             ServerSessionType
	hs            HandshakeServer
	chunkComposer *ChunkComposer
	packer        *MessagePacker

	conn         connection.Connection
	prevConnStat connection.Stat
	staleStat    *connection.Stat
	stat         base.StatSession

	// only for PubSession
	avObserver PubSessionObserver

	// only for SubSession
	IsFresh bool
}

func NewServerSession(observer ServerSessionObserver, conn net.Conn) *ServerSession {
//func NewServerSession(observer ServerSessionObserver, conn quic.Stream) *ServerSession {
	uk := base.GenUKRTMPServerSession()
	s := &ServerSession{
		conn: connection.New(conn, func(option *connection.Option) {
			option.ReadBufSize = readBufSize
		}),
		stat: base.StatSession{
			Protocol:   base.ProtocolRTMP,
			SessionID:  uk,
			StartTime:  time.Now().Format("2006-01-02 15:04:05.999"),
			//RemoteAddr: conn.RemoteAddr().String(),
		},
		uniqueKey:     uk,
		observer:      observer,
		t:             ServerSessionTypeUnknown,
		chunkComposer: NewChunkComposer(),
		packer:        NewMessagePacker(),
		IsFresh:       true,
	}
	//nazalog.Infof("[%s] lifecycle new rtmp ServerSession. session=%p, remote addr=%s", uk, s, conn.RemoteAddr().String())
	return s
}

func (s *ServerSession) RunLoop() (err error) {
	initLog("server_qoe.log")
	if err = s.handshake(); err != nil {
		return err
	}
	//cx add:for measurement
	go func() {
		var(
			total_rd_bitrate float64
			total_wr_bitrate float64
			dur 	time.Duration
		)
		iter := 0
		total_rd_bitrate =float64(0.0)
		total_wr_bitrate=float64(0.0)
		
		t := time.Now()
		now := time.Now()
		
		dur = now.Sub(t)
		t = now 
		s.UpdateStatFloat(dur.Seconds())
		for{
			now = time.Now()
			dur = now.Sub(t)
			t = now 
			s.UpdateStatFloat(dur.Seconds())
			stat := s.GetStat()
			
			nazalog.Debugf("rd bitrate:%f Mbps",stat.ReadBitrateF)
			nazalog.Debugf("wr bitrate:%f Mbps",stat.WriteBitrateF)
			time.Sleep(time.Second)
			iter += 1
			total_rd_bitrate += stat.ReadBitrateF
			total_wr_bitrate += stat.WriteBitrateF
			if(iter%10==0){
					nazalog.Infof("mean read bitrate:%f Mbps",total_rd_bitrate/float64(iter))
					nazalog.Infof("mean write bitrate:%f Mbps",total_wr_bitrate/float64(iter))
			}
			}
	}()
	return s.runReadLoop()
}

func (s *ServerSession) Write(msg []byte) error {
	_, err := s.conn.Write(msg)
	return err
}

func (s *ServerSession) Flush() error {
	return s.conn.Flush()
}

func (s *ServerSession) Dispose() error {
	nazalog.Infof("[%s] lifecycle dispose rtmp ServerSession.", s.uniqueKey)
	return s.conn.Close()
}

func (s *ServerSession) URL() string {
	return s.url
}

func (s *ServerSession) AppName() string {
	return s.appName
}

func (s *ServerSession) StreamName() string {
	return s.streamName
}

func (s *ServerSession) RawQuery() string {
	return s.rawQuery
}

func (s *ServerSession) UniqueKey() string {
	return s.uniqueKey
}

func (s *ServerSession) UpdateStat(intervalSec uint32) {
	currStat := s.conn.GetStat()
	rDiff := currStat.ReadBytesSum - s.prevConnStat.ReadBytesSum
	s.stat.ReadBitrate = int(rDiff * 8 / 1024 / uint64(intervalSec))
	wDiff := currStat.WroteBytesSum - s.prevConnStat.WroteBytesSum
	s.stat.WriteBitrate = int(wDiff * 8 / 1024 / uint64(intervalSec))
	switch s.t {
	case ServerSessionTypePub:
		s.stat.Bitrate = s.stat.ReadBitrate
	case ServerSessionTypeSub:
		s.stat.Bitrate = s.stat.WriteBitrate
	}
	s.prevConnStat = currStat
}

//cx add
func (s *ServerSession) UpdateStatFloat(intervalSec float64) {
	
	currStat := s.conn.GetStat()
	rDiff := currStat.ReadBytesSum - s.prevConnStat.ReadBytesSum
	s.stat.ReadBitrateF = float64(rDiff) * 8 / 1024 / 1024/ intervalSec
	wDiff := currStat.WroteBytesSum - s.prevConnStat.WroteBytesSum
	s.stat.WriteBitrateF = float64(wDiff) * 8 / 1024 / 1024 / intervalSec
	switch s.t {
	case ServerSessionTypePub:
		s.stat.Bitrate = s.stat.ReadBitrate
	case ServerSessionTypeSub:
		s.stat.Bitrate = s.stat.WriteBitrate
	}
	s.prevConnStat = currStat
}

func (s *ServerSession) GetStat() base.StatSession {
	connStat := s.conn.GetStat()
	s.stat.ReadBytesSum = connStat.ReadBytesSum
	s.stat.WroteBytesSum = connStat.WroteBytesSum
	return s.stat
}

func (s *ServerSession) IsAlive() (readAlive, writeAlive bool) {
	currStat := s.conn.GetStat()
	if s.staleStat == nil {
		s.staleStat = new(connection.Stat)
		*s.staleStat = currStat
		return true, true
	}

	readAlive = !(currStat.ReadBytesSum-s.staleStat.ReadBytesSum == 0)
	writeAlive = !(currStat.WroteBytesSum-s.staleStat.WroteBytesSum == 0)
	*s.staleStat = currStat
	return
}

func (s *ServerSession) runReadLoop() error {
	return s.chunkComposer.RunLoop(s.conn, s.doMsg)
}

func (s *ServerSession) handshake() error {
	if err := s.hs.ReadC0C1(s.conn); err != nil {
		return err
	}
	nazalog.Infof("[%s] < R Handshake C0+C1.", s.uniqueKey)

	nazalog.Infof("[%s] > W Handshake S0+S1+S2.", s.uniqueKey)
	if err := s.hs.WriteS0S1S2(s.conn); err != nil {
		return err
	}

	if err := s.hs.ReadC2(s.conn); err != nil {
		return err
	}
	nazalog.Infof("[%s] < R Handshake C2.", s.uniqueKey)
	return nil
}

func (s *ServerSession) doMsg(stream *Stream) error {
	//log.Debugf("%d %d %v", stream.header.msgTypeID, stream.msgLen, stream.header)
	switch stream.header.MsgTypeID {
	case base.RTMPTypeIDSetChunkSize:
		// noop
		// 因为底层的 chunk composer 已经处理过了，这里就不用处理
	case base.RTMPTypeIDCommandMessageAMF0:
		return s.doCommandMessage(stream)
	case base.RTMPTypeIDCommandMessageAMF3:
		return s.doCommandAFM3Message(stream)
	case base.RTMPTypeIDMetadata:
		return s.doDataMessageAMF0(stream)
	case base.RTMPTypeIDAck:
		return s.doACK(stream)
	case base.RTMPTypeIDAudio:
		fallthrough
	case base.RTMPTypeIDVideo:
		if s.t != ServerSessionTypePub {
			nazalog.Errorf("[%s] read audio/video message but server session not pub type.", s.uniqueKey)
			return ErrRTMP
		}
		s.avObserver.OnReadRTMPAVMsg(stream.toAVMsg())
	default:
		nazalog.Warnf("[%s] read unknown message. typeid=%d, %s", s.uniqueKey, stream.header.MsgTypeID, stream.toDebugString())

	}
	return nil
}

func (s *ServerSession) doACK(stream *Stream) error {
	seqNum := bele.BEUint32(stream.msg.buf[stream.msg.b:stream.msg.e])
	nazalog.Infof("[%s] < R Acknowledgement. ignore. sequence number=%d.", s.uniqueKey, seqNum)
	return nil
}

func (s *ServerSession) doDataMessageAMF0(stream *Stream) error {
	if s.t != ServerSessionTypePub {
		nazalog.Errorf("[%s] read audio/video message but server session not pub type.", s.uniqueKey)
		return ErrRTMP
	}

	val, err := stream.msg.peekStringWithType()
	if err != nil {
		return err
	}

	switch val {
	case "|RtmpSampleAccess":
		nazalog.Debugf("[%s] < R |RtmpSampleAccess, ignore.", s.uniqueKey)
		return nil
	default:
	}
	s.avObserver.OnReadRTMPAVMsg(stream.toAVMsg())
	return nil

	// TODO chef: 下面注释掉的代码包含的逻辑：
	// 1. 去除metadata中@setDataFrame
	// 2. 判断一些错误格式
	// 如果这个逻辑不是必须的，就可以删掉了
	// 另外，如果返回给上层的msg是删除了内容的buf，应该注意和header中的len保持一致
	//
	//switch val {
	//case "|RtmpSampleAccess":
	//	nazalog.Warnf("[%s] read data message, ignore it. val=%s", s.uniqueKey, val)
	//	return nil
	//case "@setDataFrame":
	//	// macos obs and ffmpeg
	//	// skip @setDataFrame
	//	val, err = stream.msg.readStringWithType()
	//
	//	val, err := stream.msg.peekStringWithType()
	//	if err != nil {
	//		return err
	//	}
	//	if val != "onMetaData" {
	//		nazalog.Errorf("[%s] read unknown data message. val=%s, %s", s.uniqueKey, val, stream.toDebugString())
	//		return ErrRTMP
	//	}
	//case "onMetaData":
	//	// noop
	//default:
	//	nazalog.Errorf("[%s] read unknown data message. val=%s, %s", s.uniqueKey, val, stream.toDebugString())
	//	return nil
	//}
	//
	//s.avObserver.OnReadRTMPAVMsg(stream.toAVMsg())
	//return nil
}

func (s *ServerSession) doCommandMessage(stream *Stream) error {
	cmd, err := stream.msg.readStringWithType()
	if err != nil {
		return err
	}
	tid, err := stream.msg.readNumberWithType()
	if err != nil {
		return err
	}

	switch cmd {
	case "connect":
		return s.doConnect(tid, stream)
	case "createStream":
		return s.doCreateStream(tid, stream)
	case "publish":
		return s.doPublish(tid, stream)
	case "play":
		return s.doPlay(tid, stream)
	case "releaseStream":
		fallthrough
	case "FCPublish":
		fallthrough
	case "FCUnpublish":
		fallthrough
	case "getStreamLength":
		fallthrough
	case "deleteStream":
		nazalog.Debugf("[%s] read command message, ignore it. cmd=%s, %s", s.uniqueKey, cmd, stream.toDebugString())
	default:
		nazalog.Errorf("[%s] read unknown command message. cmd=%s, %s", s.uniqueKey, cmd, stream.toDebugString())
	}
	return nil
}

func (s *ServerSession) doCommandAFM3Message(stream *Stream) error {
	//去除前面的0就是AMF0的数据
	stream.msg.consumed(1)
	return s.doCommandMessage(stream)
}

func (s *ServerSession) doConnect(tid int, stream *Stream) error {
	val, err := stream.msg.readObjectWithType()
	if err != nil {
		return err
	}
	s.appName, err = val.FindString("app")
	if err != nil {
		return err
	}
	s.tcURL, err = val.FindString("tcUrl")
	if err != nil {
		nazalog.Warnf("[%s] tcUrl not exist.", s.uniqueKey)
	}
	nazalog.Infof("[%s] < R connect('%s'). tcUrl=%s", s.uniqueKey, s.appName, s.tcURL)

	s.observer.OnRTMPConnect(s, val)

	nazalog.Infof("[%s] > W Window Acknowledgement Size %d.", s.uniqueKey, windowAcknowledgementSize)
	if err := s.packer.writeWinAckSize(s.conn, windowAcknowledgementSize); err != nil {
		return err
	}

	nazalog.Infof("[%s] > W Set Peer Bandwidth.", s.uniqueKey)
	if err := s.packer.writePeerBandwidth(s.conn, peerBandwidth, peerBandwidthLimitTypeDynamic); err != nil {
		return err
	}

	nazalog.Infof("[%s] > W SetChunkSize %d.", s.uniqueKey, LocalChunkSize)
	if err := s.packer.writeChunkSize(s.conn, LocalChunkSize); err != nil {
		return err
	}

	nazalog.Infof("[%s] > W _result('NetConnection.Connect.Success').", s.uniqueKey)
	oe, err := val.FindNumber("objectEncoding")
	if oe != 0 && oe != 3 {
		oe = 0
	}
	if err := s.packer.writeConnectResult(s.conn, tid, oe); err != nil {
		return err
	}
	return nil
}

func (s *ServerSession) doCreateStream(tid int, stream *Stream) error {
	nazalog.Infof("[%s] < R createStream().", s.uniqueKey)
	nazalog.Infof("[%s] > W _result().", s.uniqueKey)
	if err := s.packer.writeCreateStreamResult(s.conn, tid); err != nil {
		return err
	}
	return nil
}

func (s *ServerSession) doPublish(tid int, stream *Stream) (err error) {
	if err = stream.msg.readNull(); err != nil {
		return err
	}
	s.streamNameWithRawQuery, err = stream.msg.readStringWithType()
	if err != nil {
		return err
	}
	ss := strings.Split(s.streamNameWithRawQuery, "?")
	s.streamName = ss[0]
	if len(ss) == 2 {
		s.rawQuery = ss[1]
	}

	s.url = fmt.Sprintf("%s/%s", s.tcURL, s.streamNameWithRawQuery)

	pubType, err := stream.msg.readStringWithType()
	if err != nil {
		return err
	}
	nazalog.Debugf("[%s] pubType=%s", s.uniqueKey, pubType)
	nazalog.Infof("[%s] < R publish('%s')", s.uniqueKey, s.streamNameWithRawQuery)

	nazalog.Infof("[%s] > W onStatus('NetStream.Publish.Start').", s.uniqueKey)
	if err := s.packer.writeOnStatusPublish(s.conn, MSID1); err != nil {
		return err
	}

	// 回复完信令后修改 connection 的属性
	s.modConnProps()

	s.t = ServerSessionTypePub
	s.observer.OnNewRTMPPubSession(s)

	return nil
}

func (s *ServerSession) doPlay(tid int, stream *Stream) (err error) {
	if err = stream.msg.readNull(); err != nil {
		return err
	}
	s.streamNameWithRawQuery, err = stream.msg.readStringWithType()
	if err != nil {
		return err
	}
	ss := strings.Split(s.streamNameWithRawQuery, "?")
	s.streamName = ss[0]
	if len(ss) == 2 {
		s.rawQuery = ss[1]
	}

	s.url = fmt.Sprintf("%s/%s", s.tcURL, s.streamNameWithRawQuery)

	nazalog.Infof("[%s] < R play('%s').", s.uniqueKey, s.streamNameWithRawQuery)
	// TODO chef: start duration reset

	if err := s.packer.writeStreamIsRecorded(s.conn, MSID1); err != nil {
		return err
	}
	if err := s.packer.writeStreamBegin(s.conn, MSID1); err != nil {
		return err
	}

	nazalog.Infof("[%s] > W onStatus('NetStream.Play.Start').", s.uniqueKey)
	if err := s.packer.writeOnStatusPlay(s.conn, MSID1); err != nil {
		return err
	}

	// 回复完信令后修改 connection 的属性
	s.modConnProps()

	s.t = ServerSessionTypeSub
	s.observer.OnNewRTMPSubSession(s)

	return nil
}

func (s *ServerSession) modConnProps() {
	s.conn.ModWriteChanSize(wChanSize)
	// TODO chef:
	// 使用合并发送
	// naza.connection 这种方式会导致最后一点数据发送不出去，我们应该使用更好的方式，比如合并发送模式下，Dispose时发送剩余数据
	//
	//s.conn.ModWriteBufSize(writeBufSize)

	switch s.t {
	case ServerSessionTypePub:
		s.conn.ModReadTimeoutMS(serverSessionReadAVTimeoutMS)
	case ServerSessionTypeSub:
		s.conn.ModWriteTimeoutMS(serverSessionWriteAVTimeoutMS)
	}
}
