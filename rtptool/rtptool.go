package rtptool

import (
	"bytes"
	"dumpPayloadFromRTP/bitreader"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"time"
)

var (
	ErrCheckInputFile  = errors.New("check input file error")
	ErrCheckOutputFile = errors.New("check output file error")
	ErrCheckRTP        = errors.New("check rtp error")
	ErrSendRTP         = errors.New("send rtp error")
	ErrSendDone        = errors.New("send rtp done")
	ErrCheckRtpLen     = errors.New("check rtp len error")
)

type ConsoleParam struct {
	OutputFile        string
	InputFile         string
	CsvFile           string
	RemoteAddr        string
	SearchBytes       string
	Verbose           bool
	ShowProgress      bool
	SendRtpCount      int
	DumpOneFrame      bool
	PsFile            string
	OutputAudioFile   string
	OutputVideoFile   string
	DumpAudio         bool
	DumpVideo         bool
	PrintPsHeader     bool
	PrintSysHeader    bool
	PrintPsm          bool
	verbose           bool
	DumpPesStartBytes bool
	DumpVideoFrameCnt int
}

type RTPDecoder struct {
	param          *ConsoleParam
	fileBuf        *[]byte
	fileSize       int
	br             bitreader.BitReader
	InputFile      *os.File
	OutputFile     *os.File
	CsvFile        *os.File
	streamSSRC     uint32
	streamPT       uint32
	firstSeqNum    uint32
	lastSeqNum     uint32
	pktCount       uint32
	writeCsvHeader bool
	conn           net.Conn
	outputData     []byte
	gotKey         bool
	psmPos         uint32
}

func NewRTPDecoder(br bitreader.BitReader, fileBuf *[]byte, fileSize int, param *ConsoleParam) *RTPDecoder {
	var conn net.Conn
	var err error
	if param.RemoteAddr != "" {
		conn, err = net.Dial("tcp", param.RemoteAddr)
		if err != nil {
			log.Println(err)
			return nil
		}
	}
	decoder := &RTPDecoder{
		fileBuf:        fileBuf,
		fileSize:       fileSize,
		param:          param,
		br:             br,
		writeCsvHeader: true,
		conn:           conn,
		outputData:     []byte{},
	}
	return decoder
}

func (decoder *RTPDecoder) OpenFiles() error {
	var err error
	decoder.InputFile, err = os.OpenFile(decoder.param.InputFile, os.O_WRONLY|os.O_CREATE, 0666)
	if err != nil {
		log.Println(err)
		return err
	}
	if decoder.param.OutputFile != "" {
		decoder.OutputFile, err = os.OpenFile(decoder.param.OutputFile, os.O_WRONLY|os.O_CREATE, 0666)
		if err != nil {
			log.Println(err)
			return err
		}
	}
	if decoder.param.CsvFile != "" {
		decoder.CsvFile, err = os.OpenFile(decoder.param.CsvFile, os.O_WRONLY|os.O_CREATE, 0666)
		if err != nil {
			log.Println(err)
			return err
		}
	}
	return nil
}

func (decoder *RTPDecoder) getPos() int64 {
	pos := decoder.br.Size() - int64(decoder.br.Len())
	return pos
}

type RTP struct {
	// version
	V uint32
	// padding,填充标志, 如果P=1，则在该报文的尾部填充一个或多个额外的八位组，它们不是有效载荷的一部分
	P uint32
	// 如果X=1，则在RTP报头后跟有一个扩展报头。
	X uint32
	// CSRC计数器，占4位，指示CSRC 标识符的个数。
	CC uint32
	// 标记，占1位，不同的有效载荷有不同的含义，对于视频，标记一帧的结束；对于音频，标记会话的开始
	M uint32
	// 有效载荷类型，占7位，用于说明RTP报文中有效载荷的类型，如GSM音频、JPEM图像等,在流媒体中大部分是用来区分音频流和视频流的，这样便于客户端进行解析
	PT uint32
	// 序列号,占16位，用于标识发送者所发送的RTP报文的序列号，每发送一个报文，序列号增1
	seqNum uint32
	// 时间戳(Timestamp)：占32位，时戳反映了该RTP报文的第一个八位组的采样时刻。接收者使用时戳来计算延迟和延迟抖动，并进行同步控制
	timestamp uint32
	// 同步信源(SSRC)标识符：占32位，用于标识同步信源。该标识符是随机选择的，参加同一视频会议的两个同步信源不能有相同的SSRC
	SSRC uint32
	// 特约信源(CSRC)标识符：每个CSRC标识符占32位，可以有0～15个。每个CSRC标识了包含在该RTP报文有效载荷中的所有特约信源。
	CSRC   []uint32
	hdrLen uint32
	rtpLen uint32
}

func (decoder *RTPDecoder) decodePkt() *RTP {
	br := decoder.br
	rtpLen, err := br.Read32(16)
	if err != nil {
		log.Println(err)
		return nil
	}
	start := decoder.getPos()
	V, _ := br.Read32(2)
	P, _ := br.Read32(1)
	X, _ := br.Read32(1)
	CC, _ := br.Read32(4)
	M, _ := br.Read32(1)
	PT, _ := br.Read32(7)
	seqNum, _ := br.Read32(16)
	timestamp, _ := br.Read32(32)
	SSRC, _ := br.Read32(32)
	for i := 0; i < int(CC); i++ {
		br.Skip(32)
	}
	end := decoder.getPos()
	rtp := &RTP{
		V:         V,
		P:         P,
		X:         X,
		CC:        CC,
		M:         M,
		PT:        PT,
		SSRC:      SSRC,
		seqNum:    seqNum,
		timestamp: timestamp,
		hdrLen:    uint32(end - start),
		rtpLen:    rtpLen,
	}
	decoder.pktCount++
	return rtp
}

func (decoder *RTPDecoder) skipInvalidBytes(rtp *RTP) error {
	br := decoder.br
	if rtp.rtpLen < rtp.hdrLen {
		log.Println("check rtp len err, rtplen:", rtp.rtpLen, "hdrlen:", rtp.hdrLen, "pktcount:", decoder.pktCount)
		return ErrCheckRtpLen
	}
	skipLen := rtp.rtpLen - rtp.hdrLen
	skipBuf := make([]byte, skipLen)
	if _, err := io.ReadAtLeast(br, skipBuf, int(skipLen)); err != nil {
		log.Println("skip invalid bytes err:", err, "skip len: ", skipLen, "rtp len:", rtp.rtpLen, "header len:", rtp.hdrLen)
		return err
	}
	return nil
}

func (decoder *RTPDecoder) isRTPValid(rtp *RTP) bool {
	if rtp.P == 1 {
		log.Println("currently don't support decode P, pktCount:", decoder.pktCount, "seqNum:", rtp.seqNum)
		return false
	}
	if rtp.X == 1 {
		log.Println("currently don't support decode X, pktCount:", decoder.pktCount, "seqNum:", rtp.seqNum)
		return false
	}
	if decoder.streamSSRC == 0 {
		decoder.streamSSRC = rtp.SSRC
	} else if rtp.SSRC != decoder.streamSSRC {
		log.Println("check SSRC error, old:", decoder.streamSSRC, "current:", rtp.SSRC,
			"pos:", decoder.getPos(), "pktCount:", decoder.pktCount, "seqNum:", rtp.seqNum)
		return false
	}
	if decoder.streamPT == 0 {
		decoder.streamPT = rtp.PT
	} else if rtp.PT != decoder.streamPT {
		log.Println("check PT error, old:", decoder.streamPT, "current:", rtp.PT)
		return false
	}
	if decoder.param.Verbose {
		log.Println("ssrc:", rtp.seqNum)
	}
	if decoder.lastSeqNum == 0 {
		log.Println("first pkt seqNum:", rtp.seqNum)
		decoder.firstSeqNum = rtp.seqNum
		decoder.lastSeqNum = rtp.seqNum
	} else if decoder.lastSeqNum+1 != rtp.seqNum {
		log.Println("check seqNum error, last:", decoder.lastSeqNum, "current:", rtp.seqNum, "pktCount:", decoder.pktCount)
		decoder.lastSeqNum = rtp.seqNum
	} else {
		decoder.lastSeqNum = rtp.seqNum
	}
	return true
}

func (decoder *RTPDecoder) saveRTPPayload(rtp *RTP) error {
	if decoder.OutputFile == nil {
		//log.Println("check outputfile err")
		return nil
	}
	br := decoder.br
	payloadLen := rtp.rtpLen - rtp.hdrLen
	payloadData := make([]byte, payloadLen)
	// TODO io.ReadAtLeast 很多组件都处理了，需要有一个统一的方案
	if _, err := io.ReadAtLeast(br, payloadData, int(payloadLen)); err != nil {
		log.Println(err)
		return err
	}
	if !decoder.gotKey {
		if decoder.isKey(payloadData) {
			decoder.gotKey = true
			pos := decoder.getPackPos(payloadData)
			data := payloadData[pos:]
			decoder.outputData = append(decoder.outputData, data...)
			log.Println("payload:", data)
		}
		return nil
	}
	decoder.outputData = append(decoder.outputData, payloadData...)
	return nil
}

func (decoder *RTPDecoder) saveRTPInfo(rtp *RTP) error {
	if decoder.CsvFile == nil {
		//log.Println("check csv file err")
		return nil
	}
	if decoder.writeCsvHeader {
		header := "P, X, CC, M, PT, SeqNum, timestamp, SSRC, RTPLen\n"
		if _, err := decoder.CsvFile.Write([]byte(header)); err != nil {
			log.Println(err)
			return err
		}
		decoder.writeCsvHeader = false
	}
	data := fmt.Sprintf("%d, %d, %d, %d, %d, %d, %d, %d, %d\n", rtp.P, rtp.X, rtp.CC, rtp.M, rtp.PT,
		rtp.seqNum, rtp.timestamp, rtp.SSRC, rtp.rtpLen)
	if _, err := decoder.CsvFile.Write([]byte(data)); err != nil {
		log.Println(err)
		return err
	}
	return nil

}

func (decoder *RTPDecoder) sendRTP(rtp *RTP) error {
	if decoder.conn == nil {
		return nil
	}
	curPos := decoder.getPos()
	// 调用这个函数时rtp已经解析完了，buf位置已经动了
	// 2个字节为rtp长度本身
	start := uint32(curPos) - rtp.hdrLen - 2
	end := start + rtp.rtpLen + 2
	data := (*decoder.fileBuf)[start:end]
	decoder.gotKey = true
	if decoder.gotKey {
		if decoder.pktCount > uint32(decoder.param.SendRtpCount) {
			return ErrSendDone
		}
		if _, err := decoder.conn.Write(data); err != nil {
			log.Println(err)
			return ErrSendRTP
		}
	} else {
		if decoder.isKey(data) {
			decoder.gotKey = true
			if _, err := decoder.conn.Write(data); err != nil {
				log.Println(err)
				return ErrSendRTP
			}
		}
	}
	// 移动buf指针
	payloadLen := rtp.rtpLen - rtp.hdrLen
	payloadData := make([]byte, payloadLen)
	if _, err := io.ReadAtLeast(decoder.br, payloadData, int(payloadLen)); err != nil {
		log.Println(err)
		return err
	}
	time.Sleep(5 * time.Millisecond)
	return nil
}

func (decoder *RTPDecoder) SearchBytes(rtp *RTP) error {
	if decoder.param.SearchBytes == "" {
		return nil
	}
	curPos := decoder.getPos()
	// 调用这个函数时rtp已经解析完了，buf位置已经动了
	// 2个字节为rtp长度本身
	start := uint32(curPos) - rtp.hdrLen - 2
	end := start + rtp.rtpLen + 2
	data := (*decoder.fileBuf)[start:end]
	sep, err := hex.DecodeString(decoder.param.SearchBytes)
	if err != nil {
		log.Println("decode hex err")
		return err
	}
	idx := bytes.Index(data, sep)
	if idx != -1 {
		t := "unknow"
		idx = bytes.Index(data, []byte{0x00, 0x00, 0x01, 0xC0})
		if idx != -1 {
			t = "audio"
		}
		idx = bytes.Index(data, []byte{0x00, 0x00, 0x01, 0xE0})
		if idx != -1 {
			t = "video"
		}
		log.Println("seqNum:", rtp.seqNum, "timestamp:", rtp.timestamp,
			"PT:", rtp.PT, "rtplen:", rtp.rtpLen, "firstSeqNum:",
			decoder.firstSeqNum, "count:", rtp.seqNum-decoder.firstSeqNum,
			"type:", t)
		os.Exit(0)
	}
	// 移动buf指针
	payloadLen := rtp.rtpLen - rtp.hdrLen
	payloadData := make([]byte, payloadLen)
	if _, err := io.ReadAtLeast(decoder.br, payloadData, int(payloadLen)); err != nil {
		log.Println(err)
		return err
	}
	return nil
}

func (decoder *RTPDecoder) DecodePkts() error {
	for decoder.getPos() < int64(decoder.fileSize) {
		if decoder.param.ShowProgress {
			fmt.Printf("\tparsing... %d/%d %d%%\r", decoder.getPos(), decoder.fileSize, (decoder.getPos()*100)/int64(decoder.fileSize))
		}
		rtp := decoder.decodePkt()
		if err := decoder.saveRTPInfo(rtp); err != nil {
			return err
		}
		if !decoder.isRTPValid(rtp) {
			if err := decoder.skipInvalidBytes(rtp); err != nil {
				return err
			}
			continue
		}
		if err := decoder.saveRTPPayload(rtp); err != nil {
			return err
		}
		if err := decoder.sendRTP(rtp); err != nil {
			return err
		}
		if err := decoder.SearchBytes(rtp); err != nil {
			return err
		}
	}
	return nil
}

func (decoder *RTPDecoder) Save() error {
	if decoder.OutputFile != nil {
		if _, err := decoder.OutputFile.Write(decoder.outputData); err != nil {
			log.Println(err)
			return err
		}
		decoder.OutputFile.Sync()
	}
	return nil
}

func (decoder *RTPDecoder) DumpStream() {
	log.Println("ssrc:", decoder.streamSSRC)
	log.Println("pt:", decoder.streamPT)
	log.Println("first seq num:", decoder.firstSeqNum)
	log.Println("last seq num:", decoder.lastSeqNum)
	log.Println("pkt count:", decoder.pktCount)
}

func (decoder *RTPDecoder) isKey(data []byte) bool {
	start := 0
	end := len(data)
	for start < end-4 {
		buf := data[start : start+4]
		packStartCode := binary.BigEndian.Uint32(buf)
		if packStartCode == 0x000001bb {
			log.Println("got psm pos:", start)
			decoder.psmPos = uint32(start)
			return true
		}
		start++
	}
	return false
}

func (decoder *RTPDecoder) getPackPos(data []byte) int {
	start := 0
	end := len(data)
	for start < end-4 {
		buf := data[start : start+4]
		packStartCode := binary.BigEndian.Uint32(buf)
		if packStartCode == 0x000001ba {
			log.Println("got pack header pos:", start)
			return start
		}
		start++
	}
	return -1
}

func (decoder *RTPDecoder) DumpOneFrame() {
	buf := *decoder.fileBuf
	start := 4
	end := len(buf) - 4
	for start < end {
		if buf[start] == 0 && buf[start+1] == 0 && buf[start+2] == 0 && buf[start+3] == 1 && buf[start+4] == 0x41 {
			break
		}
		start++
	}
	data := buf[:start]
	decoder.outputData = append(decoder.outputData, data...)
}

func (decoder *RTPDecoder) pcapToRTPs(input, output string) (string, error) {
	cmdstr := fmt.Sprintf("tshark -nlr %s -qz \"follow,tcp,raw,0\" | tail -n +7 | sed 's/^\\s\\+//g' | xxd -r -p > %s", input, output)
	cmd := exec.Command("bash", "-c", cmdstr)
	b, err := cmd.CombinedOutput()
	if err != nil {
		log.Println("cmd:", cmdstr, "err:", err)
		return "", err
	}
	return string(b), nil
}

// rtp包转为mpg
func (decoder *RTPDecoder) rtpsToMPG(input, output string) {
}

func (decoder *RTPDecoder) decodeOneH264(h264 []byte) {

}

func (decoder *RTPDecoder) dumpFrames(input string) (string, error) {
	cmdstr := fmt.Sprintf("ffprobe -show_frames -of xml %s", input)
	cmd := exec.Command("bash", "-c", cmdstr)
	b, err := cmd.CombinedOutput()
	if err != nil {
		log.Println("cmd:", cmdstr, "err:", err)
		return "", err
	}
	return string(b), nil
}

func (decoder *RTPDecoder) dumpPackets(input string) (string, error) {
	cmdstr := fmt.Sprintf("ffprobe -show_packets -show_data -of xml %s", input)
	cmd := exec.Command("bash", "-c", cmdstr)
	b, err := cmd.CombinedOutput()
	if err != nil {
		log.Println("cmd:", cmdstr, "err:", err)
		return "", err
	}
	return string(b), nil
}
