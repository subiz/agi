package agi

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// State describes the Asterisk channel state.  There are mapped
// directly to the Asterisk enumerations.
type State int

const (
	// StateDown indicates the channel is down and available
	StateDown State = iota

	// StateReserved indicates the channel is down but reserved
	StateReserved

	// StateOffhook indicates that the channel is offhook
	StateOffhook

	// StateDialing indicates that digits have been dialed
	StateDialing

	// StateRing indicates the channel is ringing
	StateRing

	// StateRinging indicates the channel's remote end is rining (the channel is receiving ringback)
	StateRinging

	// StateUp indicates the channel is up
	StateUp

	// StateBusy indicates the line is busy
	StateBusy

	// StateDialingOffHook indicates digits have been dialed while offhook
	StateDialingOffHook

	// StatePreRing indicates the channel has detected an incoming call and is waiting for ring
	StatePreRing
)

// AGI represents an AGI session
type AGI struct {
	// Variables stored the initial variables
	// transmitted from Asterisk at the start
	// of the AGI session.
	Variables map[string]string

	r    io.Reader
	eagi io.Reader
	w    io.Writer

	conn net.Conn

	mu sync.Mutex
}

// Response represents a response to an AGI
// request.
type Response struct {
	Error        error  // Error received, if any
	Status       int    // HTTP-style status code received
	Result       int    // Result is the numerical return (if parseable)
	ResultString string // Result value as a string
	Value        string // Value is the (optional) string value returned
}

// Res returns the ResultString of a Response, as well as any error encountered.  Depending on the command, this is sometimes more useful than Val()
func (r *Response) Res() (string, error) {
	return r.ResultString, r.Error
}

// Err returns the error value from the response
func (r *Response) Err() error {
	return r.Error
}

// Val returns the response value and error
func (r *Response) Val() (string, error) {
	return r.Value, r.Error
}

// Regex for AGI response result code and value
var responseRegex = regexp.MustCompile(`^([\d]{3})\sresult=(\-?[[:alnum:]]*)(\s.*)?$`)

// ErrHangup indicates the channel hung up during processing
var ErrHangup = errors.New("hangup")

const (
	// StatusOK indicates the AGI command was
	// accepted.
	StatusOK = 200

	// StatusInvalid indicates Asterisk did not
	// understand the command.
	StatusInvalid = 510

	// StatusDeadChannel indicates that the command
	// cannot be performed on a dead (hungup) channel.
	StatusDeadChannel = 511

	// StatusEndUsage indicates...TODO
	StatusEndUsage = 520
)

// HandlerFunc is a function which accepts an AGI instance
type HandlerFunc func(*AGI)

// New creates an AGI session from the given reader and writer.
func New(r io.Reader, w io.Writer) *AGI {
	return NewWithEAGI(r, w, nil)
}

// NewWithEAGI returns a new AGI session to the given `os.Stdin` `io.Reader`,
// EAGI `io.Reader`, and `os.Stdout` `io.Writer`. The initial variables will
// be read in.
func NewWithEAGI(r io.Reader, w io.Writer, eagi io.Reader) *AGI {
	a := AGI{
		Variables: make(map[string]string),
		r:         r,
		w:         w,
		eagi:      eagi,
	}

	s := bufio.NewScanner(a.r)
	for s.Scan() {
		if s.Text() == "" {
			break
		}

		terms := strings.SplitN(s.Text(), ":", 2)
		if len(terms) == 2 {
			a.Variables[strings.TrimSpace(terms[0])] = strings.TrimSpace(terms[1])
		}
	}

	return &a
}

// NewConn returns a new AGI session bound to the given net.Conn interface
func NewConn(conn net.Conn) *AGI {
	a := New(conn, conn)
	a.conn = conn
	return a
}

// NewStdio returns a new AGI session to stdin and stdout.
func NewStdio() *AGI {
	return New(os.Stdin, os.Stdout)
}

// NewEAGI returns a new AGI session to stdin, the EAGI stream (FD=3), and stdout.
func NewEAGI() *AGI {
	return NewWithEAGI(os.Stdin, os.Stdout, os.NewFile(uintptr(3), "/dev/stdeagi"))
}

// Listen binds an AGI HandlerFunc to the given TCP `host:port` address, creating a FastAGI service.
func Listen(addr string, handler HandlerFunc) error {
	if addr == "" {
		addr = "localhost:4573"
	}

	l, err := net.Listen("tcp", addr)
	if err != nil {
		return errors.New("failed to bind server: " + err.Error())
	}
	defer l.Close() // nolint: errcheck

	for {
		conn, err := l.Accept()
		if err != nil {
			return errors.New("failed to accept TCP connection: " + err.Error())
		}

		go handler(NewConn(conn))
	}
}

func (a *AGI) IsClosed() bool {
	if a == nil {
		return true
	}
	return a.conn == nil
}

// Close closes any network connection associated with the AGI instance
func (a *AGI) Close() (err error) {
	if a.conn != nil {
		err = a.conn.Close()
		a.conn = nil
	}
	return
}

// EAGI enables access to the EAGI incoming stream (if available).
func (a *AGI) EAGI() io.Reader {
	return a.eagi
}

// Command sends the given command line to stdout
// and returns the response.
// TODO: this does not handle multi-line responses properly
func (a *AGI) Command(timeout time.Duration, cmd ...string) (resp *Response) {
	resp = &Response{}
	cmdString := strings.Join(cmd, " ")
	var raw string

	a.mu.Lock()
	defer a.mu.Unlock()

	_, err := a.w.Write([]byte(cmdString + "\n"))
	if err != nil {
		resp.Error = errors.New("failed to send command: " + err.Error())
		return
	}

	waitC := make(chan string, 1)
	go func() {
		defer func() {
			waitC <- "ok"
		}()

		s := bufio.NewScanner(a.r)
		for s.Scan() {
			raw = s.Text()
			if raw == "" {
				break
			}

			// ignore hangup signal, we dont handle it here
			if strings.HasPrefix(raw, "HANGUP") {
				continue
			}

			// Parse and store the result code
			pieces := responseRegex.FindStringSubmatch(raw)
			if pieces == nil {
				resp.Error = fmt.Errorf("failed to parse result: %s", raw)
				break
			}

			// Status code is the first substring
			resp.Status, err = strconv.Atoi(pieces[1])
			if err != nil {
				resp.Error = errors.New("failed to get status code: " + err.Error() + ", raw: " + raw)
				break
			}

			// Result code is the second substring
			resp.ResultString = pieces[2]
			resp.Result, err = strconv.Atoi(pieces[2])
			if err != nil {
				resp.Error = errors.New("failed to parse result-code as an integer: " + err.Error() + ", raw: " + raw)
			}

			// Value is the third (and optional) substring
			wrappedVal := strings.TrimSpace(pieces[3])
			resp.Value = strings.TrimSuffix(strings.TrimPrefix(wrappedVal, "("), ")")

			// FIXME: handle multiple line return values
			break // nolint
		}
	}()

	if timeout > 0 {
		select {
		case <-waitC:
		case <-time.After(timeout):
			resp.Error = fmt.Errorf("timeout")
			return
		}
	} else {
		<-waitC
	}

	// If the Status code is not 200, return an error
	if resp.Status != StatusOK && resp.Error == nil {
		resp.Error = fmt.Errorf("Non-200 status code. " + raw)
	}
	return
}

// Answer answers the channel
func (a *AGI) Answer() error {
	return a.Command(30*time.Second, "ANSWER").Err()
}

// Status returns the channel status
func (a *AGI) Status() (State, error) {
	r, err := a.Command(5*time.Second, "CHANNEL STATUS").Val()
	if err != nil {
		return StateDown, err
	}
	state, err := strconv.Atoi(r)
	if err != nil {
		return StateDown, fmt.Errorf("failed to parse state %s", r)
	}
	return State(state), nil
}

// Exec runs a dialplan application
func (a *AGI) Exec(timeout time.Duration, cmd ...string) (string, error) {
	cmd = append([]string{"EXEC"}, cmd...)
	return a.Command(timeout, cmd...).Val()
}

// Get gets the value of the given channel variable
func (a *AGI) Get(key string) (string, error) {
	return a.Command(5*time.Second, "GET VARIABLE", key).Val()
}

// GetData plays a file and receives DTMF, returning the received digits
func (a *AGI) GetData(sound string, timeout time.Duration, maxdigits int) (digits string, err error) {
	if sound == "" {
		sound = "silence/1"
	}
	resp := a.Command(0, "GET DATA", sound, toMSec(timeout), strconv.Itoa(maxdigits))
	return resp.Res()
}

// Hangup terminates the call
func (a *AGI) Hangup() error {
	return a.Command(1*time.Second, "HANGUP").Err()
}

// RecordOptions describes the options available when recording
type RecordOptions struct {
	// Format is the format of the audio file to record; defaults to "wav".
	Format string

	// EscapeDigits is the set of digits on receipt of which will terminate the recording. Default is "#".  This may not be blank.
	EscapeDigits string

	// Timeout is the maximum time to allow for the recording.  Defaults to 5 minutes.
	Timeout time.Duration

	// Silence is the maximum amount of silence to allow before ending the recording.  The finest resolution is to the second.   0=disabled, which is the default.
	Silence time.Duration

	// Beep controls whether a beep is played before starting the recording.  Defaults to false.
	Beep bool

	// Offset is the number of samples in the recording to advance before storing to the file.  This is means of clipping the beginning of a recording.  Defaults to 0.
	Offset int
}

// Record records audio to a file
func (a *AGI) Record(name string, opts *RecordOptions) error {
	if opts == nil {
		opts = &RecordOptions{}
	}
	if opts.Format == "" {
		opts.Format = "wav"
	}
	if opts.EscapeDigits == "" {
		opts.EscapeDigits = "#"
	}
	if opts.Timeout == 0 {
		opts.Timeout = 5 * time.Minute
	}

	cmd := strings.Join([]string{
		"RECORD FILE ",
		name,
		opts.Format,
		opts.EscapeDigits,
		toMSec(opts.Timeout),
	}, " ")

	if opts.Offset > 0 {
		cmd += " " + strconv.Itoa(opts.Offset)
	}

	if opts.Beep {
		cmd += " BEEP"
	}

	if opts.Silence > 0 {
		cmd += " s=" + toSec(opts.Silence)
	}

	return a.Command(0, cmd).Err()
}

// SayAlpha plays a character string, annunciating each character.
func (a *AGI) SayAlpha(label string, escapeDigits string) (digit string, err error) {
	// NOTE: AGI needs empty double quotes hold the place of the empty value in the line
	if escapeDigits == "" {
		escapeDigits = `""`
	}
	return a.Command(0, "SAY ALPHA", label, escapeDigits).Val()
}

// SayDigits plays a digit string, annunciating each digit.
func (a *AGI) SayDigits(number string, escapeDigits string) (digit string, err error) {
	// NOTE: AGI needs empty double quotes hold the place of the empty value in the line
	if escapeDigits == "" {
		escapeDigits = `""`
	}
	return a.Command(0, "SAY DIGITS", number, escapeDigits).Val()
}

// SayDate plays a date
func (a *AGI) SayDate(when time.Time, escapeDigits string) (digit string, err error) {
	// NOTE: AGI needs empty double quotes hold the place of the empty value in the line
	if escapeDigits == "" {
		escapeDigits = `""`
	}
	return a.Command(0, "SAY DATE", toEpoch(when), escapeDigits).Val()
}

// SayDateTime plays a date using the given format.  See `voicemail.conf` for the format syntax; defaults to `ABdY 'digits/at' IMp`.
func (a *AGI) SayDateTime(when time.Time, escapeDigits string, format string) (digit string, err error) {
	// Extract the timezone from the time
	zone, _ := when.Zone()

	// NOTE: AGI needs empty double quotes hold the place of the empty value in the line
	if escapeDigits == "" {
		escapeDigits = `""`
	}

	// Use the Asterisk default format if we are not given one
	if format == "" {
		format = "ABdY 'digits/at' IMp"
	}

	return a.Command(0, "SAY DATETIME", toEpoch(when), escapeDigits, format, zone).Val()
}

// SayNumber plays the given number.
func (a *AGI) SayNumber(number string, escapeDigits string) (digit string, err error) {
	// NOTE: AGI needs empty double quotes hold the place of the empty value in the line
	if escapeDigits == "" {
		escapeDigits = `""`
	}
	return a.Command(0, "SAY NUMBER", number, escapeDigits).Val()
}

// SayPhonetic plays the given phrase phonetically
func (a *AGI) SayPhonetic(phrase string, escapeDigits string) (digit string, err error) {
	// NOTE: AGI needs empty double quotes hold the place of the empty value in the line
	if escapeDigits == "" {
		escapeDigits = `""`
	}
	return a.Command(0, "SAY PHOENTIC", phrase, escapeDigits).Val()
}

// SayTime plays the time part of the given timestamp
func (a *AGI) SayTime(when time.Time, escapeDigits string) (digit string, err error) {
	// NOTE: AGI needs empty double quotes hold the place of the empty value in the line
	if escapeDigits == "" {
		escapeDigits = `""`
	}
	return a.Command(0, "SAY TIME", toEpoch(when), escapeDigits).Val()
}

// Set sets the given channel variable to
// the provided value.
func (a *AGI) Set(key, val string) error {
	return a.Command(5*time.Second, "SET VARIABLE", key, val).Err()
}

// StreamFile plays the given file to the channel
func (a *AGI) StreamFile(name string, escapeDigits string, offset int) (digit string, err error) {
	// NOTE: AGI needs empty double quotes hold the place of the empty value in the line
	if escapeDigits == "" {
		escapeDigits = `""`
	}
	return a.Command(60*time.Second, "STREAM FILE", name, escapeDigits, strconv.Itoa(offset)).Val()
}

// Verbose logs the given message to the verbose message system
func (a *AGI) Verbose(msg string, level int) error {
	return a.Command(0, "VERBOSE", strconv.Quote(msg), strconv.Itoa(level)).Err()
}

// Verbosef logs the formatted verbose output
func (a *AGI) Verbosef(format string, args ...interface{}) error {
	return a.Verbose(fmt.Sprintf(format, args...), 9)
}

// WaitForDigit waits for a DTMF digit and returns what is received
func (a *AGI) WaitForDigit(timeout time.Duration) (digit string, err error) {
	resp := a.Command(0, "WAIT FOR DIGIT", toMSec(timeout))
	resp.ResultString = ""
	if resp.Error == nil && strconv.IsPrint(rune(resp.Result)) {
		resp.ResultString = string(resp.Result)
	}
	return resp.Res()
}
