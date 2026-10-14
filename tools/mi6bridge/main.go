// mi6bridge moves in-call audio between the gateway and the voice DSP on
// MSM8998 (Xiaomi Mi 6) through raw ALSA PCM, bypassing AudioRecord and
// AudioTrack: on this platform the Android path either opens the wrong HAL
// usecase (VOICE_DOWNLINK -> afe-proxy-record) or plays the agent on the
// speaker instead of the uplink.
//
// Capture PCM (MultiMedia9 + VOC_REC_DL) is written to stdout, stdin is played
// into the playback PCM (MultiMedia2 + Incall_Music).  Both are raw S16_LE
// mono at -rate.  The mixer routes are set by the caller, not here.
//
// Runs until stdin closes, stdout breaks or SIGTERM.  Logs go to stderr.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

type interval struct{ Min, Max, Flags uint32 }
type hwParams struct {
	Flags                                        uint32
	Masks                                        [3][8]uint32
	ReservedMasks                                [5][8]uint32
	Intervals                                    [12]interval
	ReservedIntervals                            [9]interval
	Rmask, Cmask, Info, Msbits, RateNum, RateDen uint32
	FifoSize                                     uint64
	Sync                                         [16]byte
	Reserved                                     [48]byte
}
type swParams struct {
	TstampMode                                         int32
	PeriodStep, SleepMin                               uint32
	AvailMin, XferAlign, StartThreshold, StopThreshold uint64
	SilenceThreshold, SilenceSize, Boundary            uint64
	Proto, TstampType                                  uint32
	Reserved                                           [56]byte
}
type transfer struct {
	Result int64
	Buf    unsafe.Pointer
	Frames uint64
}

const (
	hwSet       = 0xc2604111
	swSet       = 0xc0884113
	prepare     = 0x4140
	drop        = 0x4143
	writeFrames = 0x40184150
	readFrames  = 0x80184151
)

func logf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, time.Now().Format("15:04:05.000 ")+format+"\n", a...)
}

func ioctl(fd int, request uintptr, arg unsafe.Pointer) error {
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), request, uintptr(arg))
	if e != 0 {
		return e
	}
	return nil
}

type pcm struct {
	fd    int
	input bool
	xruns atomic.Int64
}

func openPCM(card, device, rate, period, periods int, input bool) (*pcm, error) {
	suffix := "p"
	if input {
		suffix = "c"
	}
	path := fmt.Sprintf("/dev/snd/pcmC%dD%d%s", card, device, suffix)
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	fail := func(stage string, e error) (*pcm, error) {
		syscall.Close(fd)
		return nil, fmt.Errorf("%s %s: %w", path, stage, e)
	}
	var h hwParams
	for i := range h.Masks {
		for j := range h.Masks[i] {
			h.Masks[i][j] = ^uint32(0)
		}
	}
	for i := range h.Intervals {
		h.Intervals[i].Max = ^uint32(0)
	}
	h.Rmask = ^uint32(0)
	for i, value := range []uint{3, 2, 0} { // RW_INTERLEAVED, S16_LE, STD
		h.Masks[i] = [8]uint32{}
		h.Masks[i][value/32] = 1 << (value % 32)
	}
	// SAMPLE_BITS, FRAME_BITS, CHANNELS, RATE, PERIOD_SIZE, PERIODS
	for param, value := range map[int]int{8: 16, 9: 16, 10: 1, 11: rate, 13: period, 15: periods} {
		h.Intervals[param-8] = interval{uint32(value), uint32(value), 4}
	}
	if err := ioctl(fd, hwSet, unsafe.Pointer(&h)); err != nil {
		return fail("HW_PARAMS", err)
	}
	buffer := uint64(h.Intervals[17-8].Min)
	if buffer == 0 {
		return fail("buffer", errors.New("zero buffer frames"))
	}
	boundary := buffer
	for boundary*2 <= uint64(math.MaxInt64)-buffer {
		boundary *= 2
	}
	start := uint64(1)
	if !input {
		// Two periods queued before the DSP starts pulling, so a late first
		// frame from the gateway does not underrun immediately.
		start = uint64(2 * period)
	}
	s := swParams{PeriodStep: 1, AvailMin: uint64(period), XferAlign: 1, StartThreshold: start, StopThreshold: buffer, Boundary: boundary}
	if err := ioctl(fd, swSet, unsafe.Pointer(&s)); err != nil {
		return fail("SW_PARAMS", err)
	}
	if err := ioctl(fd, prepare, nil); err != nil {
		return fail("PREPARE", err)
	}
	logf("OPEN %s rate=%d period=%d buffer=%d", path, rate, period, buffer)
	return &pcm{fd: fd, input: input}, nil
}

func (p *pcm) close() { _ = ioctl(p.fd, drop, nil); _ = syscall.Close(p.fd) }

// transfer moves exactly len(data) frames, pacing on the PCM itself.
func (p *pcm) transfer(ctx context.Context, data []int16) error {
	op := uintptr(writeFrames)
	if p.input {
		op = readFrames
	}
	for offset := 0; offset < len(data); {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		t := transfer{Buf: unsafe.Pointer(&data[offset]), Frames: uint64(len(data) - offset)}
		err := ioctl(p.fd, op, unsafe.Pointer(&t))
		runtime.KeepAlive(data)
		if err == nil && t.Result < 0 {
			err = syscall.Errno(-t.Result)
		}
		switch {
		case errors.Is(err, syscall.EPIPE):
			p.xruns.Add(1)
			if err = ioctl(p.fd, prepare, nil); err != nil {
				return fmt.Errorf("re-prepare after xrun: %w", err)
			}
			continue
		case errors.Is(err, syscall.EAGAIN), errors.Is(err, syscall.EINTR), err == nil && t.Result == 0:
			time.Sleep(2 * time.Millisecond)
			continue
		case err != nil:
			return err
		}
		offset += int(t.Result)
	}
	return nil
}

// fifo holds agent audio between stdin and the playback PCM, bounded so a
// burst from the network cannot turn into permanent delay.
type fifo struct {
	mu      sync.Mutex
	buf     []int16
	limit   int
	dropped int64
}

func (f *fifo) push(s []int16) {
	f.mu.Lock()
	f.buf = append(f.buf, s...)
	if over := len(f.buf) - f.limit; over > 0 {
		f.buf = f.buf[over:]
		f.dropped += int64(over)
	}
	f.mu.Unlock()
}

// pop fills out from the queue and pads with silence; returns frames taken.
func (f *fifo) pop(out []int16) int {
	f.mu.Lock()
	n := copy(out, f.buf)
	f.buf = f.buf[n:]
	f.mu.Unlock()
	for i := n; i < len(out); i++ {
		out[i] = 0
	}
	return n
}

// decimator low-passes and downsamples capture taken at the DSP's native rate.
// Asking the record front-end for 8 kHz directly leaves the conversion to the
// DSP, and wideband (VoLTE) speech came out with hiss on the sibilants.
type decimator struct {
	factor int
	taps   []float64
	hist   []float64
	pos    int
}

func newDecimator(inRate, outRate int) *decimator {
	factor := inRate / outRate
	n := 16*factor + 1
	cutoff := 0.45 * float64(outRate) / float64(inRate) // cycles/sample, ~3.6 kHz at 8k out
	taps := make([]float64, n)
	sum := 0.0
	for i := range taps {
		m := float64(i - n/2)
		sinc := 2 * cutoff
		if m != 0 {
			sinc = math.Sin(2*math.Pi*cutoff*m) / (math.Pi * m)
		}
		// Blackman window
		w := 0.42 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(n-1)) + 0.08*math.Cos(4*math.Pi*float64(i)/float64(n-1))
		taps[i] = sinc * w
		sum += taps[i]
	}
	for i := range taps {
		taps[i] /= sum
	}
	return &decimator{factor: factor, taps: taps, hist: make([]float64, n)}
}

// process converts len(in) input samples into len(in)/factor output samples.
func (d *decimator) process(in []int16, out []int16) {
	n := len(d.hist)
	o := 0
	for i, v := range in {
		d.hist[d.pos] = float64(v)
		d.pos = (d.pos + 1) % n
		if (i+1)%d.factor != 0 {
			continue
		}
		acc := 0.0
		for k, t := range d.taps {
			acc += t * d.hist[(d.pos+k)%n]
		}
		if acc > 32767 {
			acc = 32767
		} else if acc < -32768 {
			acc = -32768
		}
		out[o] = int16(math.Round(acc))
		o++
	}
}

func rms(data []int16) float64 {
	var sum float64
	for _, s := range data {
		sum += float64(s) * float64(s)
	}
	return math.Sqrt(sum / float64(len(data)))
}

func run() error {
	card := flag.Int("card", 0, "ALSA card")
	inDev := flag.Int("in", 27, "capture PCM (27 = MultiMedia9)")
	outDev := flag.Int("out", 1, "playback PCM (1 = MultiMedia2)")
	rate := flag.Int("rate", 8000, "sample rate on stdin/stdout: 8000 or 16000")
	capRate := flag.Int("cap-rate", 48000, "capture PCM rate, a multiple of -rate")
	periodMs := flag.Int("period-ms", 20, "period length")
	maxQueueMs := flag.Int("max-queue-ms", 200, "agent audio kept ahead of playback")
	flag.Parse()
	if unsafe.Sizeof(hwParams{}) != 608 || unsafe.Sizeof(swParams{}) != 136 || unsafe.Sizeof(transfer{}) != 24 {
		return errors.New("requires the 64-bit ALSA ABI")
	}
	if *rate != 8000 && *rate != 16000 {
		return errors.New("rate must be 8000 or 16000")
	}
	if *capRate%*rate != 0 {
		return errors.New("-cap-rate must be a multiple of -rate")
	}
	period := *rate * *periodMs / 1000
	capPeriod := *capRate * *periodMs / 1000

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGPIPE)
	defer cancel()

	out, err := openPCM(*card, *outDev, *rate, period, 4, false)
	if err != nil {
		return err
	}
	defer out.close()
	in, err := openPCM(*card, *inDev, *capRate, capPeriod, 4, true)
	if err != nil {
		return err
	}
	defer in.close()

	queue := &fifo{limit: *rate * *maxQueueMs / 1000}
	var captured, played, starved atomic.Int64
	var peakRMS atomic.Uint64
	errs := make(chan error, 3)

	// stdin -> queue
	go func() {
		raw := make([]byte, period*2)
		for {
			n, e := io.ReadFull(os.Stdin, raw)
			if n >= 2 {
				s := make([]int16, n/2)
				for i := range s {
					s[i] = int16(uint16(raw[2*i]) | uint16(raw[2*i+1])<<8)
				}
				queue.push(s)
			}
			if e != nil {
				errs <- fmt.Errorf("stdin: %w", e)
				return
			}
		}
	}()

	// capture PCM -> stdout
	dec := newDecimator(*capRate, *rate)
	go func() {
		c := make([]int16, capPeriod)
		s := make([]int16, period)
		raw := make([]byte, period*2)
		for {
			if e := in.transfer(ctx, c); e != nil {
				errs <- fmt.Errorf("capture: %w", e)
				return
			}
			if dec.factor == 1 {
				copy(s, c)
			} else {
				dec.process(c, s)
			}
			for i, v := range s {
				raw[2*i] = byte(v)
				raw[2*i+1] = byte(uint16(v) >> 8)
			}
			if _, e := os.Stdout.Write(raw); e != nil {
				errs <- fmt.Errorf("stdout: %w", e)
				return
			}
			captured.Add(int64(period))
			if r := uint64(rms(s)); r > peakRMS.Load() {
				peakRMS.Store(r)
			}
		}
	}()

	// queue -> playback PCM, paced by the PCM: silence when nothing is queued
	go func() {
		s := make([]int16, period)
		for {
			if queue.pop(s) < period {
				starved.Add(1)
			}
			if e := out.transfer(ctx, s); e != nil {
				errs <- fmt.Errorf("playback: %w", e)
				return
			}
			played.Add(int64(period))
		}
	}()

	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			logf("STOP signal")
			return nil
		case e := <-errs:
			if errors.Is(e, io.EOF) || errors.Is(e, io.ErrUnexpectedEOF) || errors.Is(e, context.Canceled) {
				logf("STOP %v", e)
				return nil
			}
			return e
		case <-tick.C:
			queue.mu.Lock()
			depth, dropped := len(queue.buf), queue.dropped
			queue.mu.Unlock()
			logf("STATS captured=%d played=%d starved=%d queue=%d dropped=%d xrun_in=%d xrun_out=%d peakRMS=%d",
				captured.Load(), played.Load(), starved.Load(), depth, dropped, in.xruns.Load(), out.xruns.Load(), peakRMS.Swap(0))
		}
	}
}

func main() {
	if err := run(); err != nil {
		logf("ERROR %v", err)
		os.Exit(1)
	}
}
