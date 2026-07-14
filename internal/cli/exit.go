package cli

type exitCoder interface {
	ExitCode() int
}

func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	if exitErr, ok := err.(exitCoder); ok {
		return exitErr.ExitCode()
	}
	return 1
}

type commandExitError struct {
	code int
}

func (e commandExitError) Error() string {
	return "command exited with status " + intString(e.code)
}

func (e commandExitError) ExitCode() int {
	return e.code
}

func intString(value int) string {
	if value == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	v := value
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}
