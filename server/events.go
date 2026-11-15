package server

import "encoding/json"

func (c *wsClient) deliver(value any) {
	data, err := json.Marshal(value)
	if err != nil || !c.enqueue(data) {
		if c.conn != nil {
			_ = c.conn.CloseNow()
		}
	}
}

// publishLocked updates the AHP projection under the controller publication lock.
func (c *sessionController) publishLocked(changes ...SessionChange) {
	if c.chat != nil {
		c.chat.syncLocked(c, changes)
	}
}

func (s *Project) broadcast(f Frame) {
	f.Session = ""
	s.send(f)
}

func (s *Project) send(f Frame) {
	if s != nil && s.host != nil {
		s.workspaceChanged(f)
	}
}
