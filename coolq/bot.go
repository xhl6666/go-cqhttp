package coolq

import (
	"bytes"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"github.com/Mrs4s/MiraiGo/binary"
	"github.com/Mrs4s/MiraiGo/client"
	"github.com/Mrs4s/MiraiGo/message"
	"github.com/Mrs4s/go-cqhttp/global"
	log "github.com/sirupsen/logrus"
	"github.com/xujiajun/nutsdb"
	"hash/crc32"
	"math/rand"
	"path"
	"sync"
	"time"
)

type CQBot struct {
	Client *client.QQClient

	events          []func(MSG)
	db              *nutsdb.DB
	friendReqCache  sync.Map
	invitedReqCache sync.Map
	joinReqCache    sync.Map
	tempMsgCache    sync.Map
}

type MSG map[string]interface{}

func NewQQBot(cli *client.QQClient, conf *global.JsonConfig) *CQBot {
	bot := &CQBot{
		Client: cli,
	}
	if conf.EnableDB {
		opt := nutsdb.DefaultOptions
		opt.Dir = path.Join("data", "db")
		opt.EntryIdxMode = nutsdb.HintBPTSparseIdxMode
		db, err := nutsdb.Open(opt)
		if err != nil {
			log.Fatalf("打开数据库失败, 如果频繁遇到此问题请清理 data/db 文件夹或关闭数据库功能。")
		}
		bot.db = db
		gob.Register(message.Sender{})
		log.Info("信息数据库初始化完成.")
	} else {
		log.Warn("警告: 信息数据库已关闭，将无法使用 [回复/撤回] 等功能。")
	}
	bot.Client.OnPrivateMessage(bot.privateMessageEvent)
	bot.Client.OnGroupMessage(bot.groupMessageEvent)
	bot.Client.OnTempMessage(bot.tempMessageEvent)
	bot.Client.OnGroupMuted(bot.groupMutedEvent)
	bot.Client.OnGroupMessageRecalled(bot.groupRecallEvent)
	bot.Client.OnFriendMessageRecalled(bot.friendRecallEvent)
	bot.Client.OnJoinGroup(bot.joinGroupEvent)
	bot.Client.OnLeaveGroup(bot.leaveGroupEvent)
	bot.Client.OnGroupMemberJoined(bot.memberJoinEvent)
	bot.Client.OnGroupMemberLeaved(bot.memberLeaveEvent)
	bot.Client.OnGroupMemberPermissionChanged(bot.memberPermissionChangedEvent)
	bot.Client.OnNewFriendRequest(bot.friendRequestEvent)
	bot.Client.OnNewFriendAdded(bot.friendAddedEvent)
	bot.Client.OnGroupInvited(bot.groupInvitedEvent)
	bot.Client.OnUserWantJoinGroup(bot.groupJoinReqEvent)
	return bot
}

func (bot *CQBot) OnEventPush(f func(m MSG)) {
	bot.events = append(bot.events, f)
}

func (bot *CQBot) GetGroupMessage(mid int32) MSG {
	if bot.db != nil {
		m := MSG{}
		err := bot.db.View(func(tx *nutsdb.Tx) error {
			e, err := tx.Get("group-messages", binary.ToBytes(mid))
			if err != nil {
				return err
			}
			buff := new(bytes.Buffer)
			buff.Write(binary.GZipUncompress(e.Value))
			return gob.NewDecoder(buff).Decode(&m)
		})
		if err == nil {
			return m
		}
		log.Warnf("获取信息时出现错误: %v id: %v", err, mid)
	}
	return nil
}

func (bot *CQBot) SendGroupMessage(groupId int64, m *message.SendingMessage) int32 {
	var newElem []message.IMessageElement
	for _, elem := range m.Elements {
		if i, ok := elem.(*message.ImageElement); ok {
			_, _ = bot.Client.UploadGroupImage(int64(rand.Intn(11451419)), i.Data)
			gm, err := bot.Client.UploadGroupImage(groupId, i.Data)
			if err != nil {
				log.Warnf("警告: 群 %v 消息图片上传失败: %v", groupId, err)
				continue
			}
			newElem = append(newElem, gm)
			continue
		}
		if i, ok := elem.(*message.VoiceElement); ok {
			gv, err := bot.Client.UploadGroupPtt(groupId, i.Data)
			if err != nil {
				log.Warnf("警告: 群 %v 消息语音上传失败: %v", groupId, err)
				continue
			}
			newElem = append(newElem, gv)
			continue
		}
		newElem = append(newElem, elem)
	}
	m.Elements = newElem
	ret := bot.Client.SendGroupMessage(groupId, m)
	return bot.InsertGroupMessage(ret)
}

func (bot *CQBot) SendPrivateMessage(target int64, m *message.SendingMessage) int32 {
	var newElem []message.IMessageElement
	for _, elem := range m.Elements {
		if i, ok := elem.(*message.ImageElement); ok {
			fm, err := bot.Client.UploadPrivateImage(target, i.Data)
			if err != nil {
				log.Warnf("警告: 私聊 %v 消息图片上传失败.", target)
				continue
			}
			newElem = append(newElem, fm)
			continue
		}
		newElem = append(newElem, elem)
	}
	m.Elements = newElem
	var id int32
	if bot.Client.FindFriend(target) != nil {
		id = bot.Client.SendPrivateMessage(target, m).Id
	} else {
		if code, ok := bot.tempMsgCache.Load(target); ok {
			id = bot.Client.SendTempMessage(code.(int64), target, m).Id
		} else {
			return -1
		}
	}
	return ToGlobalId(target, id)
}

func (bot *CQBot) InsertGroupMessage(m *message.GroupMessage) int32 {
	val := MSG{
		"message-id":  m.Id,
		"internal-id": m.InternalId,
		"group":       m.GroupCode,
		"group-name":  m.GroupName,
		"sender":      m.Sender,
		"time":        m.Time,
		"message":     ToStringMessage(m.Elements, m.GroupCode, true),
	}
	id := ToGlobalId(m.GroupCode, m.Id)
	if bot.db != nil {
		err := bot.db.Update(func(tx *nutsdb.Tx) error {
			buf := new(bytes.Buffer)
			if err := gob.NewEncoder(buf).Encode(val); err != nil {
				return err
			}
			return tx.Put("group-messages", binary.ToBytes(id), binary.GZipCompress(buf.Bytes()), 0)
		})
		if err != nil {
			log.Warnf("记录聊天数据时出现错误: %v", err)
			return -1
		}
	}
	return id
}

func ToGlobalId(code int64, msgId int32) int32 {
	return int32(crc32.ChecksumIEEE([]byte(fmt.Sprintf("%d-%d", code, msgId))))
}

func (bot *CQBot) Release() {
	if bot.db != nil {
		_ = bot.db.Close()
	}
}

func (bot *CQBot) dispatchEventMessage(m MSG) {
	for _, f := range bot.events {
		fn := f
		go func() {
			start := time.Now()
			fn(m)
			end := time.Now()
			if end.Sub(start) > time.Second*5 {
				log.Debugf("警告: 事件处理耗时超过 5 秒 (%v), 请检查应用是否有堵塞.", end.Sub(start))
			}
		}()
	}
}

func formatGroupName(group *client.GroupInfo) string {
	return fmt.Sprintf("%s(%d)", group.Name, group.Code)
}

func formatMemberName(mem *client.GroupMemberInfo) string {
	return fmt.Sprintf("%s(%d)", mem.DisplayName(), mem.Uin)
}

func (m MSG) ToJson() string {
	b, _ := json.Marshal(m)
	return string(b)
}
