//go:build testing

package systems

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/blang/semver"
	"github.com/fxamacker/cbor/v2"
	"github.com/henrygd/beszel/internal/common"
	esystem "github.com/henrygd/beszel/internal/entities/system"
	"github.com/henrygd/beszel/internal/hub/ws"
	"github.com/lxzan/gws"
	"github.com/pocketbase/pocketbase/tools/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// topProcessesClient answers GetTopProcesses with sample, or swallows every
// request if sample is nil, as an agent without the handler does over WebSocket.
type topProcessesClient struct {
	gws.BuiltinEventHandler
	sample *esystem.TopProcesses
}

func (c *topProcessesClient) OnMessage(conn *gws.Conn, message *gws.Message) {
	defer message.Close()
	var req common.HubRequest[cbor.RawMessage]
	if err := cbor.Unmarshal(message.Bytes(), &req); err != nil || req.Action != common.GetTopProcesses || c.sample == nil {
		return
	}
	data, _ := cbor.Marshal(c.sample)
	response, _ := cbor.Marshal(common.AgentResponse{Id: req.Id, Data: data})
	_ = conn.WriteMessage(gws.OpcodeBinary, response)
}

func newTopProcessesTestSystem(t *testing.T, client *topProcessesClient) *System {
	t.Helper()
	connections := make(chan *ws.WsConn, 1)
	upgrader := gws.NewUpgrader(&monitorSyncServer{}, nil)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r)
		if err != nil {
			t.Error(err)
			return
		}
		wsConn := ws.NewWsConnection(conn, semver.MustParse("0.21.1"))
		conn.Session().Store("wsConn", wsConn)
		connections <- wsConn
		conn.ReadLoop()
	}))
	t.Cleanup(server.Close)

	conn, _, err := gws.NewClient(client, &gws.ClientOption{Addr: "ws" + strings.TrimPrefix(server.URL, "http")})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.NetConn().Close() })
	go conn.ReadLoop()

	select {
	case wsConn := <-connections:
		return &System{WsConn: wsConn}
	case <-time.After(3 * time.Second):
		t.Fatal("websocket connection was not established")
		return nil
	}
}

func TestFetchTopProcessesFromAgent(t *testing.T) {
	t.Setenv("DISABLE_SSH", "true")

	t.Run("returns the agent's sample", func(t *testing.T) {
		sample := esystem.TopProcesses{
			ByCpu:    []esystem.ProcessUsage{{Pid: 519, Name: "syspolicyd", Cpu: 67.8, Rss: 40 << 20}},
			SampleMs: 500, Scanned: 1, Total: 1,
		}
		sys := newTopProcessesTestSystem(t, &topProcessesClient{sample: &sample})
		got, err := sys.FetchTopProcessesFromAgent()
		require.NoError(t, err)
		assert.Equal(t, sample, got)
	})

	t.Run("gives up on an agent that does not answer", func(t *testing.T) {
		original := topProcessesRequestTimeout
		topProcessesRequestTimeout = 50 * time.Millisecond
		t.Cleanup(func() { topProcessesRequestTimeout = original })

		sys := newTopProcessesTestSystem(t, &topProcessesClient{})
		start := time.Now()
		_, err := sys.FetchTopProcessesFromAgent()
		require.Error(t, err)
		assert.Less(t, time.Since(start), 2*time.Second)
		assert.True(t, sys.WsConn.IsConnected(), "an unanswered sample must not close the connection")
	})

	t.Run("does not dial a system with no open connection", func(t *testing.T) {
		sys := &System{}
		_, err := sys.FetchTopProcessesFromAgent()
		assert.ErrorIs(t, err, errNoAgentConnection)
	})

	t.Run("unknown system", func(t *testing.T) {
		sm := &SystemManager{systems: store.New(map[string]*System{})}
		_, err := sm.FetchTopProcesses("missing")
		assert.Error(t, err)
	})
}
