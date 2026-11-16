package node

import (
	"reflect"
	"testing"
)

func TestDropPortFlag(t *testing.T) {
	for _, c := range []struct{ in, want []string }{
		{[]string{"start", "-p", "3000"}, []string{"start"}},
		{[]string{"start", "--port", "3001", "-H", "0.0.0.0"}, []string{"start", "-H", "0.0.0.0"}},
		{[]string{"start", "--port=3002"}, []string{"start"}},
		{[]string{"start"}, []string{"start"}},
	} {
		if got := dropPortFlag(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("dropPortFlag(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}
