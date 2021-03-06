package ucfg

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
)

type reference struct {
	Path cfgPath
}

type expansion struct {
	left, right varEvaler
	pathSep     string
	op          string
}

type splice struct {
	pieces []varEvaler
}

type varEvaler interface {
	eval(cfg *Config, opts *options) (string, error)
}

type constExp string

type token struct {
	typ tokenType
	val string
}

type parseState struct {
	st     int
	isvar  bool
	op     string
	pieces [2][]varEvaler
}

var (
	errUnterminatedBrace = errors.New("unterminated brace")
	errInvalidType       = errors.New("invalid type")
	errEmptyPath         = errors.New("empty path after expansion")
)

type tokenType uint16

const (
	tokOpen tokenType = iota
	tokClose
	tokSep
	tokString

	// parser state
	stLeft  = 0
	stRight = 1

	opDefault     = ":"
	opAlternative = ":+"
	opError       = ":?"
)

var (
	openToken  = token{tokOpen, "${"}
	closeToken = token{tokClose, "}"}

	sepDefToken = token{tokSep, opDefault}
	sepAltToken = token{tokSep, opAlternative}
	sepErrToken = token{tokSep, opError}
)

func newReference(p cfgPath) *reference {
	return &reference{p}
}

func (r *reference) String() string {
	return fmt.Sprintf("${%v}", r.Path)
}

func (r *reference) resolve(cfg *Config, opts *options) (value, error) {
	env := opts.env
	var err error

	for {
		var v value
		cfg = cfgRoot(cfg)
		if cfg == nil {
			return nil, ErrMissing
		}

		v, err = r.Path.GetValue(cfg, opts)
		if err == nil {
			if v == nil {
				break
			}
			return v, nil
		}

		if len(env) == 0 {
			break
		}

		cfg = env[len(env)-1]
		env = env[:len(env)-1]
	}

	// try callbacks
	if len(opts.resolvers) > 0 {
		key := r.Path.String()
		for i := len(opts.resolvers) - 1; i >= 0; i-- {
			var v string
			resolver := opts.resolvers[i]
			v, err = resolver(key)
			if err == nil {
				return newString(context{field: key}, nil, v), nil
			}
		}
	}

	return nil, err
}

func (r *reference) eval(cfg *Config, opts *options) (string, error) {
	v, err := r.resolve(cfg, opts)
	if err != nil {
		return "", err
	}
	if v == nil {
		return "", fmt.Errorf("can not resolve reference: %v", r.Path)
	}
	return v.toString(opts)
}

func (s constExp) eval(*Config, *options) (string, error) {
	return string(s), nil
}

func (s *splice) String() string {
	return fmt.Sprintf("%v", s.pieces)
}

func (s *splice) eval(cfg *Config, opts *options) (string, error) {
	buf := bytes.NewBuffer(nil)
	for _, p := range s.pieces {
		s, err := p.eval(cfg, opts)
		if err != nil {
			return "", err
		}
		buf.WriteString(s)
	}
	return buf.String(), nil
}

func (e *expansion) String() string {
	if e.right != nil {
		return fmt.Sprintf("${%v:%v}", e.left, e.right)
	}
	return fmt.Sprintf("${%v}", e.left)
}

func (e *expansion) eval(cfg *Config, opts *options) (string, error) {
	switch e.op {
	case opDefault:
		path, err := e.left.eval(cfg, opts)
		if err != nil || path == "" {
			return e.right.eval(cfg, opts)
		}
		ref := newReference(parsePath(path, e.pathSep))
		v, err := ref.eval(cfg, opts)
		if err != nil || v == "" {
			return e.right.eval(cfg, opts)
		}
		return v, err

	case opAlternative:
		path, err := e.left.eval(cfg, opts)
		if err != nil || path == "" {
			return "", nil
		}

		ref := newReference(parsePath(path, e.pathSep))
		tmp, err := ref.resolve(cfg, opts)
		if err != nil || tmp == nil {
			return "", nil
		}

		return e.right.eval(cfg, opts)

	case opError:
		path, err := e.left.eval(cfg, opts)
		if err == nil && path != "" {
			ref := newReference(parsePath(path, e.pathSep))
			str, err := ref.eval(cfg, opts)
			if err == nil && str != "" {
				return str, nil
			}
		}

		errStr, err := e.right.eval(cfg, opts)
		if err != nil {
			return "", err
		}
		return "", errors.New(errStr)

	case "":
		path, err := e.left.eval(cfg, opts)
		if err != nil {
			return "", err
		}

		ref := newReference(parsePath(path, e.pathSep))
		return ref.eval(cfg, opts)
	}

	return "", fmt.Errorf("Unknown expansion op: %v", e.op)
}

func (st parseState) finalize(pathSep string) (varEvaler, error) {
	if !st.isvar {
		return nil, errors.New("fatal: processing non-variable state")
	}
	if len(st.pieces[stLeft]) == 0 {
		return nil, errors.New("empty expansion")
	}

	if st.st == stLeft {
		pieces := st.pieces[stLeft]

		if len(pieces) == 0 {
			return constExp(""), nil
		}

		if len(pieces) == 1 {
			if str, ok := pieces[0].(constExp); ok {
				return newReference(parsePath(string(str), pathSep)), nil
			}
		}

		return &expansion{&splice{pieces}, nil, pathSep, ""}, nil
	}

	extract := func(pieces []varEvaler) varEvaler {
		switch len(pieces) {
		case 0:
			return constExp("")
		case 1:
			return pieces[0]
		default:
			return &splice{pieces}
		}
	}
	left := extract(st.pieces[stLeft])
	right := extract(st.pieces[stRight])
	return &expansion{left, right, pathSep, st.op}, nil
}

func parseSplice(in, pathSep string) (varEvaler, error) {
	lex, errs := lexer(in)
	defer func() {
		// on parser error drain lexer so go-routine won't leak
		for range lex {
		}
	}()

	pieces, perr := parseVarExp(lex, pathSep)

	// check for lexer errors
	err := <-errs
	if err != nil {
		return nil, err
	}

	// return parser result
	return pieces, perr
}

func lexer(in string) (<-chan token, <-chan error) {
	lex := make(chan token, 1)
	errors := make(chan error, 1)

	go func() {
		off := 0
		content := in

		defer func() {
			if len(content) > 0 {
				lex <- token{tokString, content}
			}
			close(lex)
			close(errors)
		}()

		strToken := func(s string) {
			if s != "" {
				lex <- token{tokString, s}
			}
		}

		varcount := 0
		for len(content) > 0 {
			idx := -1
			if varcount == 0 {
				idx = strings.IndexAny(content[off:], "$")
			} else {
				idx = strings.IndexAny(content[off:], "$:}")
			}
			if idx < 0 {
				return
			}

			idx += off
			off = idx + 1
			switch content[idx] {
			case ':':
				if len(content) <= off { // found ':' at end of string
					return
				}

				strToken(content[:idx])
				switch content[off] {
				case '+':
					off++
					lex <- sepAltToken
				case '?':
					off++
					lex <- sepErrToken
				default:
					lex <- sepDefToken
				}

			case '}':
				strToken(content[:idx])
				lex <- closeToken
				varcount--

			case '$':
				if len(content) <= off { // found '$' at end of string
					return
				}

				switch content[off] {
				case '$': // escape '$' symbol
					content = content[:off] + content[off+1:]
					continue
				case '{': // start variable
					strToken(content[:idx])
					lex <- openToken
					off++
					varcount++
				}
			}

			content = content[off:]
			off = 0
		}
	}()

	return lex, errors
}

func parseVarExp(lex <-chan token, pathSep string) (varEvaler, error) {
	stack := []parseState{
		parseState{st: stLeft},
	}

	// parser loop
	for tok := range lex {
		switch tok.typ {
		case tokOpen:
			stack = append(stack, parseState{st: stLeft, isvar: true})
		case tokClose:
			// finalize and pop state
			piece, err := stack[len(stack)-1].finalize(pathSep)
			stack = stack[:len(stack)-1]
			if err != nil {
				return nil, err
			}

			// append result top stacked state
			st := &stack[len(stack)-1]
			st.pieces[st.st] = append(st.pieces[st.st], piece)

		case tokSep: // switch from left to right
			st := &stack[len(stack)-1]
			if !st.isvar {
				return nil, errors.New("default separator not within expansion")
			}
			if st.st == stRight {
				return nil, errors.New("unexpected ':'")
			}
			st.st = stRight
			st.op = tok.val

		case tokString:
			// append raw string
			st := &stack[len(stack)-1]
			st.pieces[st.st] = append(st.pieces[st.st], constExp(tok.val))
		}
	}

	// validate and return final state
	if len(stack) > 1 {
		return nil, errors.New("missing '}'")
	}
	if len(stack) == 0 {
		return nil, errors.New("fatal: expansion parse state empty")
	}

	result := stack[0].pieces[stLeft]
	if len(result) == 1 {
		return result[0], nil
	}
	return &splice{result}, nil
}

func cfgRoot(cfg *Config) *Config {
	if cfg == nil {
		return nil
	}

	for {
		p := cfg.Parent()
		if p == nil {
			return cfg
		}

		cfg = p
	}
}
