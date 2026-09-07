package imapbackend

import (
	"context"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
)

type (
	user    = User
	mailbox = MailboxView
)

// UserSession is an authenticated IMAP session tied to one User. It
// implements the "authenticated" and "selected" state parts of
// imapserver.Session; Login itself lives on serverSession, since that's
// the only piece that differs between the not-authenticated and
// authenticated states.
type UserSession struct {
	*user
	*mailbox // nil until a mailbox is selected
}

func newUserSession(u *User) *UserSession {
	return &UserSession{user: u}
}

func (sess *UserSession) Close() error {
	if sess != nil && sess.mailbox != nil {
		sess.mailbox.Close()
	}
	return nil
}

func (sess *UserSession) Select(name string, options *imap.SelectOptions) (*imap.SelectData, error) {
	mbox, err := sess.user.mailbox(name)
	if err != nil {
		return nil, err
	}
	mbox.mu.Lock()
	defer mbox.mu.Unlock()
	sess.mailbox = mbox.NewView()
	return mbox.selectDataLocked(), nil
}

func (sess *UserSession) Unselect() error {
	sess.mailbox.Close()
	sess.mailbox = nil
	return nil
}

func (sess *UserSession) Copy(numSet imap.NumSet, destName string) (*imap.CopyData, error) {
	dest, err := sess.user.mailbox(destName)
	if err != nil {
		return nil, &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeTryCreate, Text: "No such mailbox"}
	}
	if sess.mailbox != nil && dest == sess.mailbox.Mailbox {
		return nil, &imap.Error{Type: imap.StatusResponseTypeNo, Text: "Source and destination mailboxes are identical"}
	}

	var sourceUIDs, destUIDs imap.UIDSet
	var copyErr error
	sess.mailbox.forEach(numSet, func(seqNum uint32, msg *message) {
		if copyErr != nil {
			return
		}
		buf, err := msg.data()
		if err != nil {
			copyErr = err
			return
		}
		appendData, err := dest.appendBytes(buf, &imap.AppendOptions{Time: msg.t, Flags: msg.flagList()})
		if err != nil {
			copyErr = err
			return
		}
		sourceUIDs.AddNum(msg.uid)
		destUIDs.AddNum(appendData.UID)
	})
	if copyErr != nil {
		return nil, copyErr
	}

	return &imap.CopyData{UIDValidity: dest.uidValidity, SourceUIDs: sourceUIDs, DestUIDs: destUIDs}, nil
}

func (sess *UserSession) Move(w *imapserver.MoveWriter, numSet imap.NumSet, destName string) error {
	dest, err := sess.user.mailbox(destName)
	if err != nil {
		return &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeTryCreate, Text: "No such mailbox"}
	}
	if sess.mailbox != nil && dest == sess.mailbox.Mailbox {
		return &imap.Error{Type: imap.StatusResponseTypeNo, Text: "Source and destination mailboxes are identical"}
	}

	sess.mailbox.mu.Lock()
	defer sess.mailbox.mu.Unlock()

	var sourceUIDs, destUIDs imap.UIDSet
	expunged := make(map[*message]struct{})
	var moveErr error
	sess.mailbox.forEachLocked(numSet, func(seqNum uint32, msg *message) {
		if moveErr != nil {
			return
		}
		buf, err := msg.data()
		if err != nil {
			moveErr = err
			return
		}
		appendData, err := dest.appendBytes(buf, &imap.AppendOptions{Time: msg.t, Flags: msg.flagList()})
		if err != nil {
			moveErr = err
			return
		}
		sourceUIDs.AddNum(msg.uid)
		destUIDs.AddNum(appendData.UID)
		expunged[msg] = struct{}{}
	})
	if moveErr != nil {
		return moveErr
	}

	for msg := range expunged {
		if err := sess.mailbox.md.Remove(msg.key); err != nil {
			return err
		}
		if err := sess.mailbox.user.backend.Store.DeleteMessageByKey(context.Background(), sess.mailbox.dbID, msg.key); err != nil {
			return err
		}
	}
	seqNums := sess.mailbox.expungeLocked(expunged)

	if err := w.WriteCopyData(&imap.CopyData{UIDValidity: dest.uidValidity, SourceUIDs: sourceUIDs, DestUIDs: destUIDs}); err != nil {
		return err
	}
	for _, seqNum := range seqNums {
		if err := w.WriteExpunge(sess.mailbox.tracker.EncodeSeqNum(seqNum)); err != nil {
			return err
		}
	}
	return nil
}

func (sess *UserSession) Poll(w *imapserver.UpdateWriter, allowExpunge bool) error {
	if sess.mailbox == nil {
		return nil
	}
	return sess.mailbox.Poll(w, allowExpunge)
}

func (sess *UserSession) Idle(w *imapserver.UpdateWriter, stop <-chan struct{}) error {
	if sess.mailbox == nil {
		<-stop
		return nil
	}
	return sess.mailbox.Idle(w, stop)
}
