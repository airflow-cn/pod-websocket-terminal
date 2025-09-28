package terminal

import (
    "context"
    "encoding/json"
    "errors"
    "fmt"
    "io"
    "time"

    "github.com/gorilla/websocket"
    v1 "k8s.io/api/core/v1"
    "k8s.io/client-go/kubernetes"
    "k8s.io/client-go/kubernetes/scheme"
    "k8s.io/client-go/rest"
    "k8s.io/client-go/tools/remotecommand"
    "k8s.io/klog/v2"
)

const (
    // Time allowed to write a message to the peer.
    writeWait = 10 * time.Second
    // ctrl+d to close terminal.
    endOfTransmission = "\u0004"

    pongWait = 30 * time.Second
    // Must be less than pongWait.
    pingPeriod = (pongWait * 9) / 10
)

// PtyHandler is what remote command expects from a pty
type PtyHandler interface {
    io.Reader
    io.Writer
    remotecommand.TerminalSizeQueue
}

// Session implements PtyHandler (using a SockJS connection)
type Session struct {
    conn     *websocket.Conn
    sizeChan chan remotecommand.TerminalSize
}

//var (
//  NodeSessionCounter sync.Map
//)

// Message is the messaging protocol between ShellController and TerminalSession.
//
// OP      DIRECTION  FIELD(S) USED  DESCRIPTION
// ---------------------------------------------------------------------
// stdin   fe->be     Data           Keystrokes/paste buffer
// resize  fe->be     Rows, Cols     New terminal size
// stdout  be->fe     Data           Output from the process
// toast   be->fe     Data           OOB message to be shown to the user
type Message struct {
    Op, Data   string
    Rows, Cols uint16
}

// Next handles pty->process resize events
// Called in a loop from remote command as long as the process is running
func (t Session) Next() *remotecommand.TerminalSize {
    size := <-t.sizeChan
    if size.Height == 0 && size.Width == 0 {
       return nil
    }
    return &size
}

// Read handles pty->process messages (stdin, resize)
// Called in a loop from remote command as long as the process is running
func (t Session) Read(p []byte) (int, error) {
    var msg Message
    if err := t.conn.ReadJSON(&msg); err != nil {
       return copy(p, endOfTransmission), err
    }

    switch msg.Op {
    case "stdin":
       return copy(p, msg.Data), nil
    case "resize":
       t.sizeChan <- remotecommand.TerminalSize{Width: msg.Cols, Height: msg.Rows}
       return 0, nil
    default:
       return copy(p, endOfTransmission), fmt.Errorf("unknown message type '%s'", msg.Op)
    }
}

// Write handles process->pty stdout
// Called from remote command whenever there is any output
func (t Session) Write(p []byte) (int, error) {
    msg, err := json.Marshal(Message{
       Op:   "stdout",
       Data: string(p),
    })
    if err != nil {
       return 0, err
    }
    if err := t.conn.SetWriteDeadline(time.Now().Add(writeWait)); err != nil {
       return 0, err
    }
    if err = t.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
       return 0, err
    }
    return len(p), nil
}

// Toast can be used to send the user any OOB messages
// term puts these in the center of the terminal
func (t Session) Toast(p string) error {
    msg, err := json.Marshal(Message{
       Op:   "toast",
       Data: p,
    })
    if err != nil {
       return err
    }
    if err := t.conn.SetWriteDeadline(time.Now().Add(writeWait)); err != nil {
       return err
    }
    if err = t.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
       return err
    }
    return nil
}

// Close shuts down the SockJS connection and sends the status code and reason to the client
// Can happen if the process exits or if there is an error starting up the process
// For now the status code is unused and reason is shown to the user (unless "")
func (t Session) Close(status uint32, reason string) {
    klog.V(4).Infof("terminal session closed: %d %s", status, reason)
    close(t.sizeChan)
    if err := t.conn.Close(); err != nil {
       klog.Warning("failed to close websocket connection: ", err)
    }
}

type Interface interface {
    HandleSession(ctx context.Context, shell, namespace, podName, containerName string, conn *websocket.Conn)
}

type terminaler struct {
    client kubernetes.Interface
    config *rest.Config
}

type NodeTerminaler struct {
    Nodename      string
    Namespace     string
    PodName       string
    ContainerName string
    Shell         string
    Privileged    bool
    client        kubernetes.Interface
}

func NewTerminaler(client kubernetes.Interface, config *rest.Config) Interface {
    return &terminaler{client: client, config: config}
}

// startProcess is called by handleAttach
// Executed cmd in the container specified in request and connects it up with the ptyHandler (a session)
func (t *terminaler) startProcess(ctx context.Context, namespace, podName, containerName string, cmd []string, ptyHandler PtyHandler) error {
    req := t.client.CoreV1().RESTClient().Post().
       Resource("pods").
       Name(podName).
       Namespace(namespace).
       SubResource("exec")
    req.VersionedParams(&v1.PodExecOptions{
       Container: containerName,
       Command:   cmd,
       Stdin:     true,
       Stdout:    true,
       Stderr:    true,
       TTY:       true,
    }, scheme.ParameterCodec)

    exec, err := remotecommand.NewSPDYExecutor(t.config, "POST", req.URL())
    if err != nil {
       return err
    }

    return exec.StreamWithContext(ctx, remotecommand.StreamOptions{
       Stdin:             ptyHandler,
       Stdout:            ptyHandler,
       Stderr:            ptyHandler,
       TerminalSizeQueue: ptyHandler,
       Tty:               true,
    })
}

// isValidShell checks if the shell is allowed
func isValidShell(validShells []string, shell string) bool {
    for _, validShell := range validShells {
       if validShell == shell {
          return true
       }
    }
    return false
}

func (t *terminaler) HandleSession(ctx context.Context, shell, namespace, podName, containerName string, conn *websocket.Conn) {
    var err error
    validShells := []string{"bash", "sh"}
    session := &Session{conn: conn, sizeChan: make(chan remotecommand.TerminalSize)}

    if isValidShell(validShells, shell) {
       cmd := []string{shell}
       err = t.startProcess(ctx, namespace, podName, containerName, cmd, session)
    } else {
       // No shell given or it was not valid: try some shells until one succeeds or all fail
       // FIXME: if the first shell fails then the first keyboard event is lost
       for _, testShell := range validShells {
          cmd := []string{testShell}
          if err = t.startProcess(ctx, namespace, podName, containerName, cmd, session); err == nil {
             break
          }
       }
    }

    if err != nil && !errors.Is(err, context.Canceled) {
       session.Close(1, err.Error())
       return
    }

    session.Close(0, "Process exited")
}
