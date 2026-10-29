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
	c.revision++
	if c.ahp != nil {
		c.ahp.syncLocked(c, changes)
	}
}
