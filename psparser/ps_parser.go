package psparser

import (
	"dumpPayloadFromRTP/bitreader"
	"dumpPayloadFromRTP/rtptool"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
)

const (
	StartCodePS    = 0x000001ba
	StartCodeSYS   = 0x000001bb
	StartCodeMAP   = 0x000001bc
	StartCodeVideo = 0x000001e0
	StartCodeAudio = 0x000001c0
)

const (
	VideoPES = 0x01
	AudioPES = 0x02
)

var (
	ErrNotFoundStartCode = errors.New("not found the need start code flag")
	ErrFormatPack        = errors.New("not package standard")
	ErrParsePakcet       = errors.New("parse ps packet error")
	ErrNewBiteReader     = errors.New("new bit reader error")
	ErrCheckH264         = errors.New("check h264 error")
	ErrCheckPayloadLen   = errors.New("check payload length error")
	ErrCheckInputFile    = errors.New("check input file error")
	ErrDumpDone          = errors.New("dump done")
)

type FieldInfo struct {
	len  uint
	item string
}

type PsDecoder struct {
	videoStreamType    uint32
	audioStreamType    uint32
	br                 bitreader.BitReader
	psHeader           map[string]uint32
	handlers           map[int]func() error
	psHeaderFields     []FieldInfo
	pktCnt             int
	fileSize           int
	psBuf              *[]byte
	errVideoFrameCnt   int
	errAudioFrameCnt   int
	totalVideoFrameCnt int
	totalAudioFrameCnt int
	iFrameCnt          int
	psmCnt             int
	errIFrameCnt       int
	pFrameCnt          int
	h264File           *os.File
	audioFile          *os.File
	param              *rtptool.ConsoleParam
}

func (dec *PsDecoder) DecodePsPkts() error {
	for dec.getPos() < int64(dec.fileSize) {
		startCode, err := dec.br.Read32(32)
		if err != nil {
			log.Println(err)
			return err
		}
		dec.pktCnt++
		if dec.param.Verbose {
			fmt.Println()
			log.Printf("pkt count: %d pos: %d/%d", dec.pktCnt, dec.getPos(), dec.fileSize)
		}
		handler, ok := dec.handlers[int(startCode)]
		if !ok {
			log.Printf("check startCode error: 0x%x pos:%d, fileSize:%d\n", startCode, dec.getPos(), dec.fileSize)
			return ErrParsePakcet
		}
		err = handler()
		if err != nil {
			return err
		}
	}
	return nil
}

func (dec *PsDecoder) decodeSystemHeader() error {
	br := dec.br
	syslens, err := br.Read32(16)
	log.Println("=== ps system header === ")
	if dec.param.PrintSysHeader {
		log.Printf("\tsystem_header_length:%d", syslens)
	}
	if err != nil {
		return err
	}

	br.Skip(uint(syslens) * 8)
	return nil
}

func (decoder *PsDecoder) getPos() int64 {
	pos := decoder.br.Size() - int64(decoder.br.Len())
	return pos
}

func (decoder *PsDecoder) decodePsmNLoop(programStreamMapLen uint32) error {
	br := decoder.br
	for programStreamMapLen > 0 {
		streamType, err := br.Read32(8)
		if decoder.param.PrintPsm {
			log.Printf("\t\tstream type: 0x%x", streamType)
		}
		if err != nil {
			return err
		}
		elementaryStreamID, err := br.Read32(8)
		if err != nil {
			return err
		}
		if elementaryStreamID >= 0xe0 && elementaryStreamID <= 0xef {
			decoder.videoStreamType = streamType
		}
		if elementaryStreamID >= 0xc0 && elementaryStreamID <= 0xdf {
			decoder.audioStreamType = streamType
		}
		if decoder.param.PrintPsm {
			log.Printf("\t\tstream id: 0x%x", elementaryStreamID)
		}
		elementaryStreamInfoLength, err := br.Read32(16)
		if err != nil {
			return err
		}
		if decoder.param.PrintPsm {
			log.Printf("\t\telementary_stream_info_length: %d", elementaryStreamInfoLength)
		}
		br.Skip(uint(elementaryStreamInfoLength * 8))
		programStreamMapLen -= (4 + elementaryStreamInfoLength)
	}
	return nil
}

func (dec *PsDecoder) decodeProgramStreamMap() error {
	br := dec.br
	dec.psmCnt++
	psmLen, err := br.Read32(16)
	if err != nil {
		return err
	}
	log.Println("=== program stream map ===")
	if dec.param.PrintPsm {
		log.Printf("\tprogram_stream_map_length: %d pos: %d", psmLen, dec.getPos())
	}
	//drop psm version info
	br.Skip(16)
	psmLen -= 2
	programStreamInfoLen, err := br.Read32(16)
	if err != nil {
		return err
	}
	br.Skip(uint(programStreamInfoLen * 8))
	psmLen -= (programStreamInfoLen + 2)
	programStreamMapLen, err := br.Read32(16)
	if err != nil {
		return err
	}
	psmLen -= (2 + programStreamMapLen)
	if dec.param.PrintPsm {
		log.Printf("\tprogram_stream_info_length: %d", programStreamMapLen)
	}

	if err := dec.decodePsmNLoop(programStreamMapLen); err != nil {
		return err
	}

	// crc 32
	if psmLen != 4 {
		if dec.param.PrintPsm {
			log.Printf("psmLen: 0x%x", psmLen)
		}
		return ErrFormatPack
	}
	br.Skip(32)
	return nil
}

func (dec *PsDecoder) decodeH264(data []byte, len uint32, err bool) error {
	if dec.param.Verbose {
		log.Printf("\t\th264 len : %d", len)
		if data[4] == 0x67 {
			log.Println("\t\tSPS")
		}
		if data[4] == 0x68 {
			log.Println("\t\tPPS")
		}
		if data[4] == 0x65 {
			log.Println("\t\tIDR")
			if err {
				dec.errIFrameCnt++
			} else {
				dec.iFrameCnt++
			}
		}
		if data[4] == 0x61 {
			log.Println("\t\tP Frame")
			dec.pFrameCnt++
		}
	}
	if !err && dec.h264File != nil {
		return dec.writeH264FrameToFile(data)
	}
	return nil
}

func (dec *PsDecoder) saveAudioPkt(data []byte, len uint32, err bool) error {
	if dec.param.Verbose {
		log.Printf("\t\taudio len : %d", len)
	}
	if !err && dec.audioFile != nil {
		dec.writeAudioFrameToFile(data)
	}
	return nil
}

func (dec *PsDecoder) isStartCodeValid(startCode uint32) bool {
	if startCode == StartCodePS ||
		startCode == StartCodeMAP ||
		startCode == StartCodeSYS ||
		startCode == StartCodeVideo ||
		startCode == StartCodeAudio {
		return true
	}
	return false
}

// 移动到当前位置+payloadLen位置，判断startcode是否正确
// 如果startcode不正确，说明payloadLen是错误的
func (dec *PsDecoder) isPayloadLenValid(payloadLen uint32, pesType int, pesStartPos int64) bool {
	psBuf := *dec.psBuf
	pos := dec.getPos() + int64(payloadLen)
	if pos >= int64(dec.fileSize) {
		log.Printf("reach file end, quit, pos: %d filesize: %d\n", pos, dec.fileSize)
		return false
	}
	packStartCode := binary.BigEndian.Uint32(psBuf[pos : pos+4])
	if !dec.isStartCodeValid(packStartCode) {
		log.Printf("check payload len error, len: %d pes start pos: %d(0x%x), pesType:%d", payloadLen, pesStartPos, pesStartPos, pesType)
		return false
	}
	return true
}

func (dec *PsDecoder) GetNextPackPos() int {
	pos := int(dec.getPos())
	for pos < dec.fileSize-4 {
		b := (*dec.psBuf)[pos : pos+4]
		packStartCode := binary.BigEndian.Uint32(b)
		if dec.isStartCodeValid((packStartCode)) {
			return pos
		}
		pos++
	}
	return dec.fileSize
}

func (dec *PsDecoder) skipInvalidBytes(payloadLen uint32, pesType int, pesStartPos int64) error {
	if pesType == VideoPES {
		dec.errVideoFrameCnt++
	} else {
		dec.errAudioFrameCnt++
	}
	br := dec.br
	pos := dec.GetNextPackPos()
	skipLen := pos - int(dec.getPos())
	log.Printf("pes start dump: % X\n", (*dec.psBuf)[pesStartPos:pesStartPos+16])
	log.Printf("pes payload len err, expect: %d actual: %d", payloadLen, skipLen)
	log.Printf("skip len: %d, next pack pos:%d", skipLen, pos)
	skipBuf := make([]byte, skipLen)
	// 由于payloadLen是错误的，所以下一个startcode和当前位置之间的字节需要丢弃
	if _, err := io.ReadAtLeast(br, skipBuf, int(skipLen)); err != nil {
		log.Println(err)
		return err
	}
	if pesType == AudioPES {
		dec.saveAudioPkt(skipBuf, uint32(skipLen), true)
	} else {
		return dec.decodeH264(skipBuf, uint32(skipLen), true)
	}
	return nil
}

func (dec *PsDecoder) decodeAudioPes() error {
	if dec.param.Verbose {
		log.Println("=== Audio ===")
	}
	dec.totalAudioFrameCnt++
	dec.decodePES(AudioPES)
	return nil
}

func (dec *PsDecoder) decodePESHeader() (uint32, error) {
	br := dec.br
	/* payload length */
	payloadLen, err := br.Read32(16)
	if err != nil {
		log.Println(err)
		return 0, err
	}

	/* flags: pts_dts_flags ... */
	br.Skip(16) // 跳过各种flags,比如pts_dts_flags
	payloadLen -= 2

	/* pes header data length */
	pesHeaderDataLen, err := br.Read32(8)
	if err != nil {
		log.Println(err)
		return 0, err
	}
	if dec.param.Verbose {
		log.Printf("\tPES_packet_length: %d", payloadLen)
		log.Printf("\tpes_header_data_length: %d", pesHeaderDataLen)
	}
	payloadLen--

	/* pes header data */
	br.Skip(uint(pesHeaderDataLen * 8))
	payloadLen -= pesHeaderDataLen
	return payloadLen, nil
}

func (dec *PsDecoder) decodePES(pesType int) error {
	br := dec.br
	pesStartPos := dec.getPos() - 4 // 4为startcode的长度
	if dec.param.DumpPesStartBytes {
		log.Printf("% X\n", (*dec.psBuf)[pesStartPos:pesStartPos+16])
	}
	payloadLen, err := dec.decodePESHeader()
	if err != nil {
		return err
	}
	if !dec.isPayloadLenValid(payloadLen, pesType, pesStartPos) {
		return dec.skipInvalidBytes(payloadLen, pesType, pesStartPos)
	}
	payloadData := make([]byte, payloadLen)
	if _, err := io.ReadAtLeast(br, payloadData, int(payloadLen)); err != nil {
		return err
	}
	if pesType == VideoPES {
		return dec.decodeH264(payloadData, payloadLen, false)
	} else {
		dec.saveAudioPkt(payloadData, payloadLen, false)
	}

	return nil
}

func (dec *PsDecoder) decodeVideoPes() error {
	if dec.param.Verbose {
		log.Println("=== video ===")
	}
	err := dec.decodePES(VideoPES)
	dec.totalVideoFrameCnt++
	return err
}

func (decoder *PsDecoder) decodePsHeader() error {
	if decoder.param.Verbose {
		log.Println("=== pack header ===")
	}
	psHeaderFields := decoder.psHeaderFields
	for _, field := range psHeaderFields {
		val, err := decoder.br.Read32(field.len)
		if err != nil {
			log.Printf("parse %s error", field.item)
			return err
		}
		decoder.psHeader[field.item] = val
	}
	pack_stuffing_length := decoder.psHeader["pack_stuffing_length"]
	decoder.br.Skip(uint(pack_stuffing_length * 8))
	if decoder.param.PrintPsHeader {
		b, err := json.MarshalIndent(decoder.psHeader, "", "  ")
		if err != nil {
			log.Println("error:", err)
		}
		fmt.Print(string(b) + "\n")
	}
	return nil
}

func (dec *PsDecoder) writeH264FrameToFile(frame []byte) error {
	if dec.totalVideoFrameCnt > dec.param.DumpVideoFrameCnt {
		return ErrDumpDone
	}
	if _, err := dec.h264File.Write(frame); err != nil {
		log.Println(err)
		return err
	}
	dec.h264File.Sync()
	return nil
}

func (dec *PsDecoder) writeAudioFrameToFile(frame []byte) error {
	if _, err := dec.audioFile.Write(frame); err != nil {
		log.Println(err)
		return err
	}
	dec.audioFile.Sync()
	return nil
}

func (dec *PsDecoder) openVideoFile() error {
	var err error
	dec.h264File, err = os.OpenFile(dec.param.OutputVideoFile, os.O_WRONLY|os.O_CREATE, 0666)
	if err != nil {
		log.Println(err)
		return err
	}
	return nil
}

func (dec *PsDecoder) openAudioFile() error {
	var err error
	dec.audioFile, err = os.OpenFile(dec.param.OutputAudioFile, os.O_WRONLY|os.O_CREATE, 0666)
	if err != nil {
		log.Println(err)
		return err
	}
	return nil
}

func NewPsDecoder(br bitreader.BitReader, psBuf *[]byte, fileSize int, param *rtptool.ConsoleParam) *PsDecoder {
	decoder := &PsDecoder{
		br:             br,
		psHeader:       make(map[string]uint32),
		handlers:       make(map[int]func() error),
		psHeaderFields: make([]FieldInfo, 14),
		fileSize:       fileSize,
		psBuf:          psBuf,
		param:          param,
	}
	decoder.handlers = map[int]func() error{
		StartCodePS:    decoder.decodePsHeader,
		StartCodeSYS:   decoder.decodeSystemHeader,
		StartCodeMAP:   decoder.decodeProgramStreamMap,
		StartCodeVideo: decoder.decodeVideoPes,
		StartCodeAudio: decoder.decodeAudioPes,
	}
	decoder.psHeaderFields = []FieldInfo{
		{2, "fixed"},
		{3, "system_clock_refrence_base1"},
		{1, "marker_bit1"},
		{15, "system_clock_refrence_base2"},
		{1, "marker_bit2"},
		{15, "system_clock_refrence_base3"},
		{1, "marker_bit3"},
		{9, "system_clock_reference_extension"},
		{1, "marker_bit4"},
		{22, "program_mux_rate"},
		{1, "marker_bit5"},
		{1, "marker_bit6"},
		{5, "reserved"},
		{3, "pack_stuffing_length"},
	}
	if param.DumpAudio {
		err := decoder.openAudioFile()
		if err != nil {
			return nil
		}
	}
	if param.DumpVideo {
		err := decoder.openVideoFile()
		if err != nil {
			return nil
		}
	}
	return decoder
}

func (dec *PsDecoder) ShowInfo() {
	fmt.Println()
	log.Printf("total video frame count: %d\n", dec.totalVideoFrameCnt)
	log.Printf("err frame cont: %d\n", dec.errVideoFrameCnt)
	log.Printf("I frame count: %d\n", dec.iFrameCnt)
	log.Printf("err I frame count: %d\n", dec.errIFrameCnt)
	log.Printf("program stream map count: %d", dec.psmCnt)
	log.Printf("P frame count: %d\n", dec.pFrameCnt)
	log.Println("total audio frame count:", dec.totalAudioFrameCnt)
	log.Printf("video stream type: 0x%x\n", dec.videoStreamType)
	log.Printf("audio stream type: 0x%x\n", dec.audioStreamType)
}
