package copy

import (
	"bytes"
	"fmt"
	"github.com/viant/afs/file"
	"github.com/viant/afs/option"
	"github.com/viant/endly"
	"github.com/viant/toolbox"
	"io"
	"io/ioutil"
	"os"
	"strings"
)

var maxExpandableContentSize = int64(1024 * 128)

//NewModifier return a new reader that can substitute content with state map, replacement data provided in replacement map.
func NewModifier(context *endly.Context, when *Matcher, replaceMap map[string]string, expand bool) (option.Modifier, error) {

	matchHandler, err := substitutionMatcher(when)
	if err != nil {
		return nil, err
	}
	return func(parent string, info os.FileInfo, reader io.ReadCloser) (os.FileInfo, io.ReadCloser, error) {
		if reader == nil {
			return nil, nil, fmt.Errorf("reader was empty")
		}
		if !matchHandler("", info) {
			return info, reader, nil
		}
		var isUpdated = false
		defer func() {
			_ = reader.Close()
		}()
		content, err := ioutil.ReadAll(reader)
		if err != nil {
			return info, nil, err
		}
		var result = string(content)
		if expand && canExpand(content) {
			result = context.Expand(result)
			isUpdated = result != string(content)
		}

		if replaced, substituted := substituteWithMap(result, replaceMap); replaced {
			result = substituted
			isUpdated = replaced
		}

		info = file.AdjustInfoSize(info, len(result))
		if isUpdated {
			return info, ioutil.NopCloser(strings.NewReader(toolbox.AsString(result))), nil
		}
		return info, ioutil.NopCloser(bytes.NewReader(content)), nil
	}, nil
}

func substitutionMatcher(matcher *Matcher) (result option.Match, err error) {
	if matcher != nil {
		if result, err = matcher.Matcher(); err != nil {
			return nil, err
		}
	}
	if result != nil {
		return result, nil
	}
	return func(parent string, info os.FileInfo) bool {
		return info.Size() < maxExpandableContentSize
	}, err
}

func substituteWithMap(text string, replaceMap map[string]string) (bool, string) {
	isUpdated := false
	for k, v := range replaceMap {
		count := strings.Count(text, k)
		if count == 0 {
			continue
		}
		if !isUpdated {
			isUpdated = true
		}
		text = strings.Replace(text, k, v, count)
	}
	return isUpdated, text
}

func canExpand(content []byte) bool {
	if len(content) == 0 {
		return false
	}
	limit := 100
	if limit >= len(content) {
		limit = len(content) - 1
	}
	return toolbox.IsPrintText(string(content[:limit]))
}
