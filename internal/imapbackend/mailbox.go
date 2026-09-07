package imapbackend

import (
	"context"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"

	"github.com/sociolytik/mailserver/internal/maildir"
	"github.com/sociolytik/mailserver/internal/store"
)

// specialUseByName maps our fixed default folder names to the IMAP
// special-use attributes clients use to place them in the right spot in
// the folder sidebar (RFC 6154).
var specialUseByName = map[string]imap.MailboxAttr{
	"Sent":   imap.MailboxAttrSent,
	"Drafts": imap.MailboxAttrDrafts,
	"Trash":  imap.MailboxAttrTrash,
	"Junk":   imap.MailboxAttrJunk,
}

// Mailbox is one IMAP folder for one user, backed by a Maildir directory on
// disk and a store.Mailbox row for UID bookkeeping. It is shared between
// every session that has it selected, matching how emersion/go-imap expects
// mailbox state to be shared so that updates propagate to all viewers.
type Mailbox struct {
	user *User
	root string
	md   *maildir.Maildir

	tracker     *imapserver.MailboxTracker
	uidValidity uint32

	mu         sync.Mutex
	dbID       int64
	name       string
	subscribed bool
	l          []*message // ordered by ascending UID
}

func newMailbox(u *User, dbmbox *store.Mailbox) (*Mailbox, error) {
	root := maildir.FolderPath(u.root(), dbmbox.Name)
	md := maildir.New(root)
	if err := md.Init(); err != nil {
		return nil, err
	}

	mbox := &Mailbox{
		user:        u,
		root:        root,
		md:          md,
		tracker:     imapserver.NewMailboxTracker(0),
		uidValidity: dbmbox.UIDValidity,
		dbID:        dbmbox.ID,
		name:        dbmbox.Name,
		subscribed:  dbmbox.Subscribed,
	}

	mbox.mu.Lock()
	err := mbox.reloadLocked(context.Background())
	mbox.mu.Unlock()
	if err != nil {
		return nil, err
	}

	return mbox, nil
}

// reloadLocked reconciles the in-memory message list with what's actually
// on disk: newly delivered files (from SMTP, or dropped in by another tool)
// get indexed and assigned a UID, files that vanished get dropped. It must
// be called with mu held.
func (mbox *Mailbox) reloadLocked(ctx context.Context) error {
	diskMsgs, err := mbox.md.List()
	if err != nil {
		return fmt.Errorf("listing maildir %s: %w", mbox.root, err)
	}

	dbMsgs, err := mbox.user.backend.Store.ListMessages(ctx, mbox.dbID)
	if err != nil {
		return err
	}
	dbByKey := make(map[string]store.MailboxMessage, len(dbMsgs))
	for _, m := range dbMsgs {
		dbByKey[m.MaildirKey] = m
	}

	diskKeys := make([]string, len(diskMsgs))
	for i, dm := range diskMsgs {
		diskKeys[i] = dm.Key
	}
	if err := mbox.user.backend.Store.DeleteMessagesNotIn(ctx, mbox.dbID, diskKeys); err != nil {
		return err
	}

	existingByKey := make(map[string]*message, len(mbox.l))
	for _, m := range mbox.l {
		existingByKey[m.key] = m
	}

	newList := make([]*message, 0, len(diskMsgs))
	for _, dm := range diskMsgs {
		if m, ok := existingByKey[dm.Key]; ok {
			newList = append(newList, m)
			continue
		}
		if dbRow, ok := dbByKey[dm.Key]; ok {
			newList = append(newList, &message{
				mbox: mbox, uid: imap.UID(dbRow.UID), key: dm.Key,
				t: dbRow.InternalDate, size: dbRow.Size, flags: decodeFlags(dbRow.Flags),
			})
			continue
		}

		info, statErr := mbox.md.Stat(dm.Key)
		var size int64
		internalDate := time.Now()
		if statErr == nil {
			size = info.Size()
			internalDate = info.ModTime()
		}
		flagSet := flagSetFromMaildirLetters(dm.Flags)

		uid, err := mbox.user.backend.Store.InsertMessage(ctx, mbox.dbID, dm.Key, dm.Flags, internalDate, size)
		if err != nil {
			return fmt.Errorf("indexing message %s: %w", dm.Key, err)
		}
		newList = append(newList, &message{
			mbox: mbox, uid: imap.UID(uid), key: dm.Key,
			t: internalDate, size: size, flags: flagSet,
		})
	}

	sort.Slice(newList, func(i, j int) bool { return newList[i].uid < newList[j].uid })

	if len(newList) != len(mbox.l) {
		mbox.tracker.QueueNumMessages(uint32(len(newList)))
	}
	mbox.l = newList

	return nil
}

func (mbox *Mailbox) persistFlagsLocked(ctx context.Context, msg *message) error {
	flags := msg.flagList()
	if err := mbox.user.backend.Store.UpdateMessageFlags(ctx, mbox.dbID, msg.key, encodeFlags(flags)); err != nil {
		return err
	}
	return mbox.md.SetFlags(msg.key, maildirLettersFromFlagList(flags))
}

func (mbox *Mailbox) countByFlagLocked(flag imap.Flag) uint32 {
	var n uint32
	for _, msg := range mbox.l {
		if msg.hasFlag(flag) {
			n++
		}
	}
	return n
}

// findByUIDLocked looks up a message by UID; used by the webmail facade
// (webapi.go), which addresses messages by UID rather than IMAP sequence
// numbers/NumSets.
func (mbox *Mailbox) findByUIDLocked(uid imap.UID) *message {
	for _, msg := range mbox.l {
		if msg.uid == uid {
			return msg
		}
	}
	return nil
}

func (mbox *Mailbox) StatusData(options *imap.StatusOptions) *imap.StatusData {
	mbox.mu.Lock()
	defer mbox.mu.Unlock()
	return mbox.statusDataLocked(options)
}

func (mbox *Mailbox) statusDataLocked(options *imap.StatusOptions) *imap.StatusData {
	_ = mbox.reloadLocked(context.Background())

	data := imap.StatusData{Mailbox: mbox.name}
	if options.NumMessages {
		num := uint32(len(mbox.l))
		data.NumMessages = &num
	}
	if options.UIDNext {
		var maxUID imap.UID
		for _, m := range mbox.l {
			if m.uid > maxUID {
				maxUID = m.uid
			}
		}
		data.UIDNext = maxUID + 1
	}
	if options.UIDValidity {
		data.UIDValidity = mbox.uidValidity
	}
	if options.NumUnseen {
		num := uint32(len(mbox.l)) - mbox.countByFlagLocked(imap.FlagSeen)
		data.NumUnseen = &num
	}
	if options.NumDeleted {
		num := mbox.countByFlagLocked(imap.FlagDeleted)
		data.NumDeleted = &num
	}
	if options.Size {
		var size int64
		for _, m := range mbox.l {
			size += m.size
		}
		data.Size = &size
	}
	if options.NumRecent {
		num := uint32(0)
		data.NumRecent = &num
	}
	return &data
}

func (mbox *Mailbox) list(options *imap.ListOptions) *imap.ListData {
	mbox.mu.Lock()
	defer mbox.mu.Unlock()

	if options.SelectSubscribed && !mbox.subscribed {
		return nil
	}
	specialUse, hasSpecialUse := specialUseByName[mbox.name]
	if options.SelectSpecialUse && !hasSpecialUse {
		return nil
	}

	data := imap.ListData{Mailbox: mbox.name, Delim: mailboxDelim}
	if mbox.subscribed {
		data.Attrs = append(data.Attrs, imap.MailboxAttrSubscribed)
	}
	if (options.ReturnSpecialUse || options.SelectSpecialUse) && hasSpecialUse {
		data.Attrs = append(data.Attrs, specialUse)
	}
	if options.ReturnStatus != nil {
		data.Status = mbox.statusDataLocked(options.ReturnStatus)
	}
	return &data
}

func (mbox *Mailbox) setSubscribed(ctx context.Context, subscribed bool) error {
	mbox.mu.Lock()
	mbox.subscribed = subscribed
	mbox.mu.Unlock()
	return mbox.user.backend.Store.SetMailboxSubscribed(ctx, mbox.dbID, subscribed)
}

func (mbox *Mailbox) selectDataLocked() *imap.SelectData {
	flags := mbox.flagsLocked()
	permanentFlags := append(append([]imap.Flag(nil), flags...), imap.FlagWildcard)

	return &imap.SelectData{
		Flags:             flags,
		PermanentFlags:    permanentFlags,
		NumMessages:       uint32(len(mbox.l)),
		FirstUnseenSeqNum: mbox.firstUnseenSeqNumLocked(),
		UIDNext:           mbox.uidNextLocked(),
		UIDValidity:       mbox.uidValidity,
	}
}

func (mbox *Mailbox) uidNextLocked() imap.UID {
	var maxUID imap.UID
	for _, m := range mbox.l {
		if m.uid > maxUID {
			maxUID = m.uid
		}
	}
	return maxUID + 1
}

func (mbox *Mailbox) firstUnseenSeqNumLocked() uint32 {
	for i, msg := range mbox.l {
		if !msg.hasFlag(imap.FlagSeen) {
			return uint32(i) + 1
		}
	}
	return 0
}

func (mbox *Mailbox) flagsLocked() []imap.Flag {
	m := make(map[imap.Flag]struct{})
	for _, msg := range mbox.l {
		for flag := range msg.flags {
			m[flag] = struct{}{}
		}
	}
	var l []imap.Flag
	for flag := range m {
		l = append(l, flag)
	}
	sort.Slice(l, func(i, j int) bool { return l[i] < l[j] })
	return l
}

func (mbox *Mailbox) appendLiteral(r imap.LiteralReader, options *imap.AppendOptions) (*imap.AppendData, error) {
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for {
		n, err := r.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
	}
	return mbox.appendBytes(buf, options)
}

func (mbox *Mailbox) appendBytes(buf []byte, options *imap.AppendOptions) (*imap.AppendData, error) {
	key, err := mbox.md.Deliver(buf)
	if err != nil {
		return nil, fmt.Errorf("delivering appended message: %w", err)
	}

	t := options.Time
	if t.IsZero() {
		t = time.Now()
	}

	ctx := context.Background()
	uid, err := mbox.user.backend.Store.InsertMessage(ctx, mbox.dbID, key, maildirLettersFromFlagList(options.Flags), t, int64(len(buf)))
	if err != nil {
		return nil, fmt.Errorf("indexing appended message: %w", err)
	}

	flagSet := make(map[imap.Flag]struct{}, len(options.Flags))
	for _, f := range options.Flags {
		flagSet[canonicalFlag(f)] = struct{}{}
	}
	if len(options.Flags) > 0 {
		if err := mbox.user.backend.Store.UpdateMessageFlags(ctx, mbox.dbID, key, encodeFlags(options.Flags)); err != nil {
			return nil, err
		}
		if err := mbox.md.SetFlags(key, maildirLettersFromFlagList(options.Flags)); err != nil {
			return nil, err
		}
	}

	msg := &message{mbox: mbox, uid: imap.UID(uid), key: key, t: t, size: int64(len(buf)), flags: flagSet}

	mbox.mu.Lock()
	mbox.l = append(mbox.l, msg)
	mbox.tracker.QueueNumMessages(uint32(len(mbox.l)))
	mbox.mu.Unlock()

	return &imap.AppendData{UIDValidity: mbox.uidValidity, UID: msg.uid}, nil
}

func (mbox *Mailbox) rename(newName string) {
	mbox.mu.Lock()
	mbox.name = newName
	mbox.mu.Unlock()
}

// NewView creates a per-connection view into this mailbox. Callers must
// call MailboxView.Close once they are done with it.
func (mbox *Mailbox) NewView() *MailboxView {
	return &MailboxView{Mailbox: mbox, tracker: mbox.tracker.NewSession()}
}

// A MailboxView is a single IMAP connection's view of a shared Mailbox,
// with its own queue of pending unilateral updates (RFC 3501's
// per-connection sequence-number space).
type MailboxView struct {
	*Mailbox
	tracker   *imapserver.SessionTracker
	searchRes imap.UIDSet
}

func (mbox *MailboxView) Close() {
	mbox.tracker.Close()
}

func (mbox *MailboxView) Fetch(w *imapserver.FetchWriter, numSet imap.NumSet, options *imap.FetchOptions) error {
	markSeen := false
	for _, bs := range options.BodySection {
		if !bs.Peek {
			markSeen = true
			break
		}
	}

	ctx := context.Background()
	var err error
	mbox.forEach(numSet, func(seqNum uint32, msg *message) {
		if err != nil {
			return
		}
		if markSeen && !msg.hasFlag(imap.FlagSeen) {
			msg.flags[imap.FlagSeen] = struct{}{}
			if persistErr := mbox.persistFlagsLocked(ctx, msg); persistErr != nil {
				err = persistErr
				return
			}
			mbox.Mailbox.tracker.QueueMessageFlags(seqNum, msg.uid, msg.flagList(), nil)
		}

		respWriter := w.CreateMessage(mbox.tracker.EncodeSeqNum(seqNum))
		err = msg.fetch(respWriter, options)
	})
	return err
}

func (mbox *MailboxView) Search(numKind imapserver.NumKind, criteria *imap.SearchCriteria, options *imap.SearchOptions) (*imap.SearchData, error) {
	mbox.mu.Lock()
	defer mbox.mu.Unlock()

	mbox.staticSearchCriteria(criteria)

	var (
		data   imap.SearchData
		seqSet imap.SeqSet
		uidSet imap.UIDSet
	)
	for i, msg := range mbox.l {
		seqNum := mbox.tracker.EncodeSeqNum(uint32(i) + 1)

		if !msg.search(seqNum, criteria) {
			continue
		}

		uidSet.AddNum(msg.uid)

		var num uint32
		switch numKind {
		case imapserver.NumKindSeq:
			if seqNum == 0 {
				continue
			}
			seqSet.AddNum(seqNum)
			num = seqNum
		case imapserver.NumKindUID:
			num = uint32(msg.uid)
		}
		if data.Min == 0 || num < data.Min {
			data.Min = num
		}
		if data.Max == 0 || num > data.Max {
			data.Max = num
		}
		data.Count++
	}

	switch numKind {
	case imapserver.NumKindSeq:
		data.All = seqSet
	case imapserver.NumKindUID:
		data.All = uidSet
	}

	if options.ReturnSave {
		mbox.searchRes = uidSet
	}

	return &data, nil
}

func (mbox *MailboxView) staticSearchCriteria(criteria *imap.SearchCriteria) {
	seqNums := make([]imap.SeqSet, 0, len(criteria.SeqNum))
	for _, seqSet := range criteria.SeqNum {
		numSet := mbox.staticNumSet(seqSet)
		switch numSet := numSet.(type) {
		case imap.SeqSet:
			seqNums = append(seqNums, numSet)
		case imap.UIDSet:
			criteria.UID = append(criteria.UID, numSet)
		}
	}
	criteria.SeqNum = seqNums

	for i, uidSet := range criteria.UID {
		criteria.UID[i] = mbox.staticNumSet(uidSet).(imap.UIDSet)
	}

	for i := range criteria.Not {
		mbox.staticSearchCriteria(&criteria.Not[i])
	}
	for i := range criteria.Or {
		for j := range criteria.Or[i] {
			mbox.staticSearchCriteria(&criteria.Or[i][j])
		}
	}
}

func (mbox *MailboxView) Store(w *imapserver.FetchWriter, numSet imap.NumSet, flags *imap.StoreFlags, options *imap.StoreOptions) error {
	ctx := context.Background()
	var err error
	mbox.forEach(numSet, func(seqNum uint32, msg *message) {
		if err != nil {
			return
		}
		msg.store(flags)
		if persistErr := mbox.persistFlagsLocked(ctx, msg); persistErr != nil {
			err = persistErr
			return
		}
		mbox.Mailbox.tracker.QueueMessageFlags(seqNum, msg.uid, msg.flagList(), mbox.tracker)
	})
	if err != nil {
		return err
	}
	if !flags.Silent {
		return mbox.Fetch(w, numSet, &imap.FetchOptions{Flags: true})
	}
	return nil
}

func (mbox *MailboxView) Expunge(w *imapserver.ExpungeWriter, uids *imap.UIDSet) error {
	ctx := context.Background()

	mbox.mu.Lock()
	expunged := make(map[*message]struct{})
	for _, msg := range mbox.l {
		if uids != nil && !uids.Contains(msg.uid) {
			continue
		}
		if msg.hasFlag(imap.FlagDeleted) {
			expunged[msg] = struct{}{}
		}
	}
	mbox.mu.Unlock()

	if len(expunged) == 0 {
		return nil
	}

	for msg := range expunged {
		if err := mbox.md.Remove(msg.key); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing message %s: %w", msg.key, err)
		}
		if err := mbox.user.backend.Store.DeleteMessageByKey(ctx, mbox.dbID, msg.key); err != nil {
			return err
		}
	}

	mbox.mu.Lock()
	seqNums := mbox.expungeLocked(expunged)
	mbox.mu.Unlock()

	for _, seqNum := range seqNums {
		encoded := mbox.tracker.EncodeSeqNum(seqNum)
		if encoded == 0 {
			continue
		}
		if err := w.WriteExpunge(encoded); err != nil {
			return err
		}
	}
	return nil
}

func (mbox *Mailbox) expungeLocked(expunged map[*message]struct{}) (seqNums []uint32) {
	var filtered []*message
	for i := len(mbox.l) - 1; i >= 0; i-- {
		msg := mbox.l[i]
		if _, ok := expunged[msg]; ok {
			seqNum := uint32(i) + 1
			seqNums = append(seqNums, seqNum)
			mbox.tracker.QueueExpunge(seqNum)
		} else {
			filtered = append(filtered, msg)
		}
	}
	for i := 0; i < len(filtered)/2; i++ {
		j := len(filtered) - i - 1
		filtered[i], filtered[j] = filtered[j], filtered[i]
	}
	mbox.l = filtered
	return seqNums
}

// Poll is called for NOOP and after most commands; it also re-scans disk so
// mail delivered by SMTP while this mailbox was selected is picked up.
func (mbox *MailboxView) Poll(w *imapserver.UpdateWriter, allowExpunge bool) error {
	mbox.mu.Lock()
	_ = mbox.reloadLocked(context.Background())
	mbox.mu.Unlock()
	return mbox.tracker.Poll(w, allowExpunge)
}

// Idle backs the IDLE command. It periodically re-scans disk so mail
// delivered by SMTP is pushed to the client without it having to poll.
func (mbox *MailboxView) Idle(w *imapserver.UpdateWriter, stop <-chan struct{}) error {
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				mbox.mu.Lock()
				_ = mbox.reloadLocked(context.Background())
				mbox.mu.Unlock()
			}
		}
	}()
	err := mbox.tracker.Idle(w, stop)
	close(done)
	return err
}

func (mbox *MailboxView) forEach(numSet imap.NumSet, f func(seqNum uint32, msg *message)) {
	mbox.mu.Lock()
	defer mbox.mu.Unlock()
	mbox.forEachLocked(numSet, f)
}

func (mbox *MailboxView) forEachLocked(numSet imap.NumSet, f func(seqNum uint32, msg *message)) {
	numSet = mbox.staticNumSet(numSet)

	for i, msg := range mbox.l {
		seqNum := uint32(i) + 1

		var contains bool
		switch numSet := numSet.(type) {
		case imap.SeqSet:
			encoded := mbox.tracker.EncodeSeqNum(seqNum)
			contains = encoded != 0 && numSet.Contains(encoded)
		case imap.UIDSet:
			contains = numSet.Contains(msg.uid)
		}
		if !contains {
			continue
		}
		f(seqNum, msg)
	}
}

// staticNumSet resolves "*" (max seq/UID) and "$" (SEARCHRES) against the
// current mailbox state, since both are relative markers rather than fixed
// numbers.
func (mbox *MailboxView) staticNumSet(numSet imap.NumSet) imap.NumSet {
	if imap.IsSearchRes(numSet) {
		return mbox.searchRes
	}

	switch numSet := numSet.(type) {
	case imap.SeqSet:
		max := uint32(len(mbox.l))
		for i := range numSet {
			r := &numSet[i]
			staticNumRange(&r.Start, &r.Stop, max)
		}
	case imap.UIDSet:
		max := uint32(mbox.uidNextLocked()) - 1
		for i := range numSet {
			r := &numSet[i]
			staticNumRange((*uint32)(&r.Start), (*uint32)(&r.Stop), max)
		}
	}
	return numSet
}

func staticNumRange(start, stop *uint32, max uint32) {
	dyn := false
	if *start == 0 {
		*start = max
		dyn = true
	}
	if *stop == 0 {
		*stop = max
		dyn = true
	}
	if dyn && *start > *stop {
		*start, *stop = *stop, *start
	}
}
