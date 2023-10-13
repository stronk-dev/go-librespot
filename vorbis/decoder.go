package vorbis

import (
	"errors"
	"fmt"
	log "github.com/sirupsen/logrus"
	librespot "go-librespot"
	"io"
	"strings"
	"sync"

	"github.com/xlab/vorbis-go/vorbis"
)

const (
	// DataChunkSize represents the amount of data read from physical bitstream on each iteration.
	DataChunkSize = 4096 // could be also 8192
)

// Decoder implements an OggVorbis decoder.
type Decoder struct {
	sync.Mutex

	// gain is the default track gain.
	gain float32

	// duration is the input length in milliseconds.
	duration int32

	// syncState tracks the synchronization of the current page. It is used during
	// decoding to track the status of data as it is read in, synchronized, verified,
	// and parsed into pages belonging to the various logical bistreams
	// in the current physical bitstream link.
	syncState vorbis.OggSyncState

	// streamState tracks the current decode state of the current logical bitstream.
	streamState vorbis.OggStreamState

	// page encapsulates the data for an Ogg page. Ogg pages are the fundamental unit
	// of framing and interleave in an Ogg bitstream.
	page vorbis.OggPage

	// packet encapsulates the data for a single raw packet of data and is used to transfer
	// data between the Ogg framing layer and the handling codec.
	packet vorbis.OggPacket

	// info contains basic information about the audio in a vorbis bitstream.
	info vorbis.Info

	// comment stores all the bitstream user comments as Ogg Vorbis comment.
	comment vorbis.Comment

	// dspState is the state for one instance of the Vorbis decoder.
	// This structure is intended to be private.
	dspState vorbis.DspState

	// block holds the data for a single block of audio. One Vorbis block translates to one codec packet.
	// The decoding process consists of decoding the packets into blocks and reassembling the audio from the blocks.
	// This structure is intended to be private.
	block vorbis.Block

	input    librespot.SizedReadAtSeeker
	pcm      [][][]float32
	buf      []float32
	stopChan chan struct{}
	closed   bool

	lastGranulepos vorbis.OggInt64
}

// Info represents basic information about the audio in a Vorbis bitstream.
type Info struct {
	Channels   int32
	SampleRate int32
	Comments   []string
	Vendor     string
}

// New creates and initialises a new OggVorbis decoder for the provided bytestream.
func New(r librespot.SizedReadAtSeeker, duration int32, gain float32) (*Decoder, error) {
	d := &Decoder{
		input:    r,
		duration: duration,
		gain:     gain,
		stopChan: make(chan struct{}),
	}

	vorbis.OggSyncInit(&d.syncState)

	if err := d.readStreamHeaders(); err != nil {
		d.decoderStateCleanup()
		return nil, err
	}

	d.pcm = [][][]float32{
		make([][]float32, d.info.Channels),
	}

	if ret := vorbis.SynthesisInit(&d.dspState, &d.info); ret < 0 {
		d.decoderStateCleanup()
		return nil, errors.New("vorbis: error during playback initialization")
	}

	vorbis.BlockInit(&d.dspState, &d.block)

	return d, nil
}

// Info returns some basic info about the Vorbis stream the decoder was fed with.
func (d *Decoder) Info() Info {
	return ReadInfo(&d.info, &d.comment)
}

// Close stops and finalizes the decoding process, releases the allocated resources.
// Puts the decoder into an unrecoverable state.
func (d *Decoder) Close() {
	if !d.stopRequested() {
		close(d.stopChan)
	}
	d.Lock()
	defer d.Unlock()
	if d.closed {
		return
	}
	d.closed = true
	d.decoderStateCleanup()
}

func (d *Decoder) decoderStateCleanup() {
	vorbis.OggStreamClear(&d.streamState)
	d.streamState.Free()

	vorbis.CommentClear(&d.comment)
	d.comment.Free()

	vorbis.InfoClear(&d.info)
	d.info.Free()

	vorbis.OggSyncClear(&d.syncState)
	d.syncState.Free()

	vorbis.DspClear(&d.dspState)
	d.dspState.Free()

	vorbis.BlockClear(&d.block)
	d.block.Free()

	vorbis.OggPacketClear(&d.packet)
	d.packet.Free()

	// clear up all remaining refs
	d.page.Free()
}

func (d *Decoder) stopRequested() bool {
	select {
	case <-d.stopChan:
		return true
	default:
		return false
	}
}

func (d *Decoder) readChunk() (n int, err error) {
	buf := vorbis.OggSyncBuffer(&d.syncState, DataChunkSize)
	n, err = io.ReadFull(d.input, buf[:DataChunkSize])
	vorbis.OggSyncWrote(&d.syncState, n)
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return n, io.EOF
	}
	return n, err
}

func (d *Decoder) readStreamHeaders() error {
	if _, err := d.readChunk(); err != nil {
		return fmt.Errorf("vorbis: failed reading chunk: %w", err)
	}

	// Read the first page
	if ret := vorbis.OggSyncPageout(&d.syncState, &d.page); ret != 1 {
		return errors.New("vorbis: not a valid Ogg bitstream")
	}

	// Init the logical bitstream with serial number stored in the page
	vorbis.OggStreamInit(&d.streamState, vorbis.OggPageSerialno(&d.page))

	vorbis.InfoInit(&d.info)
	vorbis.CommentInit(&d.comment)

	// Add a complete page to the bitstream
	if ret := vorbis.OggStreamPagein(&d.streamState, &d.page); ret < 0 {
		return errors.New("vorbis: the supplied page does not belong this Vorbis stream")
	}
	// Get the first packet
	if ret := vorbis.OggStreamPacketout(&d.streamState, &d.packet); ret != 1 {
		return errors.New("vorbis: unable to fetch initial Vorbis packet from the first page")
	}
	// Finally decode the header packet
	if ret := vorbis.SynthesisHeaderin(&d.info, &d.comment, &d.packet); ret < 0 {
		return fmt.Errorf("vorbis: unable to decode the initial Vorbis header: %d", ret)
	}

	var headersRead int
forPage:
	for headersRead < 2 {
		if res := vorbis.OggSyncPageout(&d.syncState, &d.page); res < 0 {
			// bytes have been skipped, try to sync again
			continue forPage
		} else if res == 0 {
			// go get more data
			if _, err := d.readChunk(); err != nil {
				return errors.New("vorbis: got EOF while reading Vorbis headers")
			}
			continue forPage
		}
		// page is synced at this point
		vorbis.OggStreamPagein(&d.streamState, &d.page)
		for headersRead < 2 {
			if ret := vorbis.OggStreamPacketout(&d.streamState, &d.packet); ret < 0 {
				return errors.New("vorbis: data is missing near the secondary Vorbis header")
			} else if ret == 0 {
				// no packets left on the page, go get a new one
				continue forPage
			}
			if ret := vorbis.SynthesisHeaderin(&d.info, &d.comment, &d.packet); ret < 0 {
				return errors.New("vorbis: unable to read the secondary Vorbis header")
			}
			headersRead++
		}
	}

	d.info.Deref()
	d.comment.Deref()
	d.comment.UserComments = make([][]byte, d.comment.Comments)
	d.comment.Deref()
	return nil
}

func (d *Decoder) Read(p []float32) (n int, err error) {
	d.Lock()
	defer d.Unlock()
	if d.closed {
		return 0, errors.New("decoder: decoder has already been closed")
	}

	n = 0
	for n < len(p) {
		// read from page buffer
		if len(d.buf) > 0 {
			copied := copy(p[n:], d.buf)
			d.buf = d.buf[copied:]
			n += copied
		}

		// decode another page
		err = d.readNextPage()
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

func (d *Decoder) safeSynthesisPcmout() (ret int32) {
	defer func() {
		err := recover()
		if err == nil {
			return
		}

		switch err := err.(type) {
		case string:
			// the calloc inside allocPPFloatMemory will sometimes fail for no apparent reason,
			// avoid panicking the entire program and fail locally instead.
			if strings.HasPrefix(err, "memory alloc error") {
				ret = -1
				return
			}
		}

		panic(err)
	}()

	return vorbis.SynthesisPcmout(&d.dspState, d.pcm)
}

func (d *Decoder) readNextPage() (err error) {
	for {
		if ret := vorbis.OggSyncPageout(&d.syncState, &d.page); ret < 0 {
			log.Debugf("vorbis: corrupt or missing data in bitstream")
			continue
		} else if ret == 0 {
			// need more data
			_, err = d.readChunk()
			if err != nil {
				return err
			}
		} else {
			// we have read the page
			break
		}
	}

	// page is synced at this point
	vorbis.OggStreamPagein(&d.streamState, &d.page)

	for {
		if ret := vorbis.OggStreamPacketout(&d.streamState, &d.packet); ret < 0 {
			// skip this packet
			continue
		} else if ret == 0 {
			// no packets left on the page
			break
		}

		if vorbis.Synthesis(&d.block, &d.packet) == 0 {
			vorbis.SynthesisBlockin(&d.dspState, &d.block)
		}

		samples := d.safeSynthesisPcmout()
		for ; samples > 0; samples = d.safeSynthesisPcmout() {
			for i := 0; i < int(samples); i++ {
				for j := 0; j < int(d.info.Channels); j++ {
					d.buf = append(d.buf, d.pcm[0][j][:samples][i]*d.gain)
				}
			}
			vorbis.SynthesisRead(&d.dspState, samples)
		}

		// save last observed position
		d.lastGranulepos = vorbis.OggPageGranulepos(&d.page)
	}

	if vorbis.OggPageEos(&d.page) == 1 {
		return io.EOF
	}

	return nil
}

func (d *Decoder) SetPositionMs(pos int64) (err error) {
	d.Lock()
	defer d.Unlock()

	_, err = d.input.Seek(pos*d.input.Size()/int64(d.duration), io.SeekStart)
	if err != nil {
		return fmt.Errorf("failed seeking input: %w", err)
	}

	// read data at seek offset
	if _, err = d.readChunk(); err != nil {
		return fmt.Errorf("failed reading chunk: %w", err)
	}

	for {
		err = d.readNextPage()
		if err != nil {
			return fmt.Errorf("failed reading page: %w", err)
		}

		if d.PositionMs() >= pos {
			break
		}
	}

	d.buf = nil
	return nil
}

func (d *Decoder) PositionMs() int64 {
	return int64(vorbis.GranuleTime(&d.dspState, d.lastGranulepos) * 1000)
}
