package coolq

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/url"
	"os"
	"path"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/ProtocolScience/AstralGo/client/nt"
	"github.com/ProtocolScience/AstralGo/client/pb/database"

	"github.com/ProtocolScience/AstralGo/message"
	"github.com/ProtocolScience/AstralGo/utils"
	b14 "github.com/fumiama/go-base16384"
	"github.com/segmentio/asm/base64"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"

	"github.com/BegoniaHe/AstralGocq/db"
	"github.com/BegoniaHe/AstralGocq/global"
	"github.com/BegoniaHe/AstralGocq/internal/base"
	"github.com/BegoniaHe/AstralGocq/internal/cache"
	"github.com/BegoniaHe/AstralGocq/internal/download"
	"github.com/BegoniaHe/AstralGocq/internal/mime"
	"github.com/BegoniaHe/AstralGocq/internal/msg"
	"github.com/BegoniaHe/AstralGocq/internal/param"
	"github.com/BegoniaHe/AstralGocq/pkg/onebot"
)

// TODO: move this file to internal/msg, internal/onebot
// TODO: support OneBot V12

const (
	maxImageSize = 1024 * 1024 * 30  // 30MB
	maxVideoSize = 1024 * 1024 * 100 // 100MB
)

func replyID(r *message.ReplyElement, source message.Source) int32 {
	id := source.PrimaryID
	seq := r.ReplySeq
	if r.GroupID != 0 {
		id = r.GroupID
	}
	// 私聊时，部分（不确定）的账号会在 ReplyElement 中带有 GroupID 字段。
	// 这里需要判断是由于 “直接回复” 功能，GroupID 为触发直接回复的来源那个群。
	if source.SourceType == message.SourcePrivate && (r.Sender == source.PrimaryID || r.GroupID == source.PrimaryID || r.GroupID == 0) {
		// 私聊似乎腾讯服务器有bug?
		seq = int32(uint16(seq))
		id = r.Sender
	}
	return db.ToGlobalID(id, seq)
}

// toElements 将消息元素数组转为MSG数组以用于消息上报
//
// nolint:govet
func toElements(e []message.IMessageElement, source message.Source) (r []msg.Element) {
	// TODO: support OneBot V12
	type pair = msg.Pair // simplify code
	type pairs = []pair

	r = make([]msg.Element, 0, len(e))
	m := &message.SendingMessage{Elements: e}
	reply := m.FirstOrNil(func(e message.IMessageElement) bool {
		_, ok := e.(*message.ReplyElement)
		return ok
	})
	if reply != nil && source.SourceType&(message.SourceGroup|message.SourcePrivate) != 0 {
		replyElem := reply.(*message.ReplyElement)
		id := replyID(replyElem, source)
		elem := msg.Element{
			Type: "reply",
			Data: pairs{
				{K: "id", V: strconv.FormatInt(int64(id), 10)},
			},
		}
		if base.ExtraReplyData {
			elem.Data = append(elem.Data,
				pair{K: "seq", V: strconv.FormatInt(int64(replyElem.ReplySeq), 10)},
				pair{K: "qq", V: strconv.FormatInt(replyElem.Sender, 10)},
				pair{K: "time", V: strconv.FormatInt(int64(replyElem.Time), 10)},
				pair{K: "text", V: toStringMessage(replyElem.Elements, source)},
			)
		}
		r = append(r, elem)
	}
	for i, elem := range e {
		var m msg.Element
		switch o := elem.(type) {
		case *message.ReplyElement:
			if base.RemoveReplyAt && i+1 < len(e) {
				elem, ok := e[i+1].(*message.AtElement)
				if ok && elem.Target == o.Sender {
					e[i+1] = nil
				}
			}
			continue
		case *message.TextElement:
			m = msg.Element{
				Type: "text",
				Data: pairs{
					{K: "text", V: o.Content},
				},
			}
		case *message.LightAppElement:
			m = msg.Element{
				Type: "json",
				Data: pairs{
					{K: "data", V: o.Content},
				},
			}
		case *message.AtElement:
			target := "all"
			if o.Target != 0 {
				target = strconv.FormatUint(uint64(o.Target), 10)
			}
			m = msg.Element{
				Type: "at",
				Data: pairs{
					{K: "qq", V: target},
				},
			}
		case *message.RedBagElement:
			m = msg.Element{
				Type: "redbag",
				Data: pairs{
					{K: "title", V: o.Title},
				},
			}
		case *message.ForwardElement:
			m = msg.Element{
				Type: "forward",
				Data: pairs{
					{K: "id", V: o.ResId},
				},
			}
		case *message.FaceElement:
			m = msg.Element{
				Type: "face",
				Data: pairs{
					{K: "id", V: strconv.FormatInt(int64(o.Index), 10)},
				},
			}
		case *message.NewTechVoiceElement:
			m = msg.Element{
				Type: "record",
				Data: pairs{
					{K: "file", V: o.FileName},
					{K: "url", V: o.Url},
					{K: "during", V: strconv.Itoa(int(o.Duration))},
				},
			}
		case *message.ShortVideoElement:
			m = msg.Element{
				Type: "video",
				Data: pairs{
					{K: "file", V: o.Name},
					{K: "url", V: o.Url},
				},
			}
		case *message.NewTechImageElement:
			switch {
			case o.LegacyFriend != nil:
				data := pairs{
					{K: "file", V: hex.EncodeToString(o.LegacyFriend.Md5) + ".image"},
					{K: "url", V: o.LegacyFriend.Url},
				}
				if o.LegacyFriend.Flash {
					data = append(data, pair{K: "type", V: "flash"})
				}
				m = msg.Element{
					Type: "image",
					Data: data,
				}
			case o.LegacyGroup != nil:
				data := pairs{
					{K: "file", V: hex.EncodeToString(o.LegacyGroup.Md5) + ".image"},
					{K: "subType", V: strconv.FormatInt(int64(o.LegacyGroup.ImageBizType), 10)},
					{K: "url", V: o.LegacyGroup.Url},
				}
				if o.LegacyGroup.EffectID != 0 { //群闪照已经下线了
					data = append(data, pair{K: "type", V: "show"})
					data = append(data, pair{K: "id", V: strconv.FormatInt(int64(o.LegacyGroup.EffectID), 10)})
				}
				m = msg.Element{
					Type: "image",
					Data: data,
				}
			case o.LegacyGuild != nil:
				data := pairs{
					{K: "file", V: hex.EncodeToString(o.LegacyGuild.Md5) + ".image"},
					{K: "url", V: o.LegacyGuild.Url},
				}
				m = msg.Element{
					Type: "image",
					Data: data,
				}
			default:
				data := pairs{
					{K: "file", V: hex.EncodeToString(o.Md5) + ".image"},
					{K: "subType", V: strconv.FormatInt(int64(o.ImageBizType), 10)},
					{K: "url", V: o.Url},
				}
				m = msg.Element{
					Type: "image",
					Data: data,
				}
			}
		case *message.DiceElement:
			m = msg.Element{
				Type: "dice",
				Data: pairs{
					{K: "value", V: strconv.FormatInt(int64(o.Value), 10)},
				},
			}
		case *message.FingerGuessingElement:
			m = msg.Element{
				Type: "rps",
				Data: pairs{
					{K: "value", V: strconv.FormatInt(int64(o.Value), 10)},
				},
			}
		case *message.MarketFaceElement:
			m = msg.Element{
				Type: "text",
				Data: pairs{
					{K: "text", V: o.Name},
				},
			}
		case *message.ServiceElement:
			m = msg.Element{
				Type: "xml",
				Data: pairs{
					{K: "data", V: o.Content},
					{K: "resid", V: o.ResId},
				},
			}
			if !strings.Contains(o.Content, "<?xml") {
				m.Type = "json"
			}
		case *message.AnimatedSticker:
			m = msg.Element{
				Type: "face",
				Data: pairs{
					{K: "id", V: strconv.FormatInt(int64(o.ID), 10)},
					{K: "type", V: "sticker"},
				},
			}
		case *message.GroupFileElement:
			m = msg.Element{
				Type: "file",
				Data: pairs{
					{K: "path", V: o.Path},
					{K: "name", V: o.Name},
					{K: "size", V: strconv.FormatInt(o.Size, 10)},
					{K: "busid", V: strconv.FormatInt(int64(o.Busid), 10)},
				},
			}
		case *msg.LocalImage:
			data := pairs{
				{K: "file", V: o.File},
				{K: "url", V: o.URL},
			}
			if o.Flash {
				data = append(data, pair{K: "type", V: "flash"})
			}
			m = msg.Element{
				Type: "image",
				Data: data,
			}
		default:
			continue
		}
		r = append(r, m)
	}
	return
}

// ToMessageContent 将消息转换成 Content. 忽略 Reply
// 不同于 onebot 的 Array Message, 此函数转换出来的 Content 的 data 段为实际类型
// 方便数据库查询
func ToMessageContent(e []message.IMessageElement, source message.Source) (r []global.MSG) {
	for _, elem := range e {
		var m global.MSG
		switch o := elem.(type) {
		case *message.ReplyElement:
			m = global.MSG{
				"type": "reply",
				"data": global.MSG{"id": replyID(o, source)},
			}
		case *message.TextElement:
			m = global.MSG{
				"type": "text",
				"data": global.MSG{"text": o.Content},
			}
		case *message.LightAppElement:
			m = global.MSG{
				"type": "json",
				"data": global.MSG{"data": o.Content},
			}
		case *message.AtElement:
			if o.Target == 0 {
				m = global.MSG{
					"type": "at",
					"data": global.MSG{
						"subType": "all",
					},
				}
			} else {
				m = global.MSG{
					"type": "at",
					"data": global.MSG{
						"subType": "user",
						"target":  o.Target,
						"display": o.Display,
					},
				}
			}
		case *message.RedBagElement:
			m = global.MSG{
				"type": "redbag",
				"data": global.MSG{"title": o.Title, "type": int(o.MsgType)},
			}
		case *message.ForwardElement:
			m = global.MSG{
				"type": "forward",
				"data": global.MSG{"id": o.ResId},
			}
		case *message.FaceElement:
			m = global.MSG{
				"type": "face",
				"data": global.MSG{"id": o.Index},
			}
		case *message.NewTechVoiceElement:
			m = global.MSG{
				"type": "record",
				"data": global.MSG{"file": o.FileName, "url": o.Url, "during": o.Duration},
			}
		case *message.ShortVideoElement:
			m = global.MSG{
				"type": "video",
				"data": global.MSG{"file": o.Name, "url": o.Url},
			}
		case *message.NewTechImageElement:
			switch {
			case o.LegacyGuild != nil:
				m = global.MSG{
					"type": "image",
					"data": global.MSG{"file": hex.EncodeToString(o.LegacyGuild.Md5) + ".image", "url": o.LegacyGuild.Url},
				}
			case o.LegacyFriend != nil:
				data := global.MSG{"file": hex.EncodeToString(o.LegacyFriend.Md5) + ".image", "url": o.LegacyFriend.Url}
				if o.LegacyFriend.Flash {
					data["type"] = "flash"
				}
				m = global.MSG{
					"type": "image",
					"data": data,
				}
			case o.LegacyGroup != nil:
				data := global.MSG{"file": hex.EncodeToString(o.Md5) + ".image", "url": o.LegacyGroup.Url, "subType": uint32(o.LegacyGroup.ImageBizType)}
				if o.LegacyGroup.EffectID != 0 {
					data["type"] = "show"
					data["id"] = o.LegacyGroup.EffectID
				}
				m = global.MSG{
					"type": "image",
					"data": data,
				}
			default:
				data := global.MSG{"file": hex.EncodeToString(o.Md5) + ".image", "url": o.Url, "subType": uint32(o.ImageBizType)}
				m = global.MSG{
					"type": "image",
					"data": data,
				}
			}
		case *message.DiceElement:
			m = global.MSG{"type": "dice", "data": global.MSG{"value": o.Value}}
		case *message.FingerGuessingElement:
			m = global.MSG{"type": "rps", "data": global.MSG{"value": o.Value}}
		case *message.MarketFaceElement:
			m = global.MSG{"type": "text", "data": global.MSG{"text": o.Name}}
		case *message.ServiceElement:
			if isOk := strings.Contains(o.Content, "<?xml"); isOk {
				m = global.MSG{
					"type": "xml",
					"data": global.MSG{"data": o.Content, "resid": o.Id},
				}
			} else {
				m = global.MSG{
					"type": "json",
					"data": global.MSG{"data": o.Content, "resid": o.Id},
				}
			}
		case *message.AnimatedSticker:
			m = global.MSG{
				"type": "face",
				"data": global.MSG{"id": o.ID, "type": "sticker"},
			}
		case *message.GroupFileElement:
			m = global.MSG{
				"type": "file",
				"data": global.MSG{"path": o.Path, "name": o.Name, "size": strconv.FormatInt(o.Size, 10), "busid": strconv.FormatInt(int64(o.Busid), 10)},
			}
		default:
			continue
		}
		r = append(r, m)
	}
	return
}

// ConvertStringMessage 将消息字符串转为消息元素数组
func (bot *CQBot) ConvertStringMessage(spec *onebot.Spec, raw string, sourceType message.SourceType) (r []message.IMessageElement) {
	elems := msg.ParseString(raw)
	return bot.ConvertElements(spec, elems, sourceType, false)
}

// ConvertObjectMessage 将消息JSON对象转为消息元素数组
func (bot *CQBot) ConvertObjectMessage(spec *onebot.Spec, m gjson.Result, sourceType message.SourceType) (r []message.IMessageElement) {
	if spec.Version == 11 && m.Type == gjson.String {
		return bot.ConvertStringMessage(spec, m.Str, sourceType)
	}
	elems := msg.ParseObject(m)
	return bot.ConvertElements(spec, elems, sourceType, false)
}

// ConvertContentMessage 将数据库用的 content 转换为消息元素数组
func (bot *CQBot) ConvertContentMessage(content []global.MSG, sourceType message.SourceType, noReply bool) (r []message.IMessageElement) {
	elems := make([]msg.Element, len(content))
	for i, v := range content {
		elem := msg.Element{Type: v["type"].(string)}
		for k, v := range v["data"].(global.MSG) {
			pair := msg.Pair{K: k, V: fmt.Sprint(v)}
			elem.Data = append(elem.Data, pair)
		}
		elems[i] = elem
	}
	return bot.ConvertElements(onebot.V11, elems, sourceType, noReply)
}

// ConvertElements 将解码后的消息数组转换为MiraiGo表示
func (bot *CQBot) ConvertElements(spec *onebot.Spec, elems []msg.Element, sourceType message.SourceType, noReply bool) (r []message.IMessageElement) {
	var replyCount int
	for _, elem := range elems {
		if noReply && elem.Type == "reply" {
			continue
		}
		me, err := bot.ConvertElement(spec, elem, sourceType)
		if err != nil {
			// TODO: don't use cqcode format
			if !base.IgnoreInvalidCQCode {
				r = append(r, message.NewText(elem.CQCode()))
			}
			log.Warnf("转换消息 %v 到MiraiGo Element时出现错误: %v.", elem.CQCode(), err)
			continue
		}
		switch i := me.(type) {
		case *message.ReplyElement:
			if replyCount > 0 {
				log.Warnf("警告: 一条信息只能包含一个 Reply 元素.")
				break
			}
			replyCount++
			// 将回复消息放置于第一个
			r = append([]message.IMessageElement{i}, r...)
		case message.IMessageElement:
			r = append(r, i)
		case []message.IMessageElement:
			r = append(r, i...)
		}
	}
	return
}

func (bot *CQBot) reply(spec *onebot.Spec, elem msg.Element, sourceType message.SourceType) (any, error) {
	mid, err := strconv.Atoi(elem.Get("id"))
	customText := elem.Get("text")
	var re *message.ReplyElement
	switch {
	case customText != "":
		var org db.StoredMessage
		sender, senderErr := strconv.ParseInt(elem.Get("user_id"), 10, 64)
		if senderErr != nil {
			sender, senderErr = strconv.ParseInt(elem.Get("qq"), 10, 64)
		}
		if senderErr != nil && err != nil {
			return nil, errors.New("警告: 自定义 reply 元素中必须包含 user_id 或 id")
		}
		msgTime, timeErr := strconv.ParseInt(elem.Get("time"), 10, 64)
		if timeErr != nil {
			msgTime = time.Now().Unix()
		}
		messageSeq, seqErr := strconv.ParseInt(elem.Get("seq"), 10, 64)
		if err == nil {
			org, _ = db.GetMessageByGlobalID(int32(mid))
		}
		if org != nil {
			re = &message.ReplyElement{
				ReplySeq: org.GetAttribute().MessageSeq,
				Sender:   org.GetAttribute().SenderUin,
				Time:     int32(org.GetAttribute().Timestamp),
				Elements: bot.ConvertStringMessage(spec, customText, sourceType),
			}
			if senderErr != nil {
				re.Sender = sender
			}
			if timeErr != nil {
				re.Time = int32(msgTime)
			}
			if seqErr != nil {
				re.ReplySeq = int32(messageSeq)
			}
			break
		}
		re = &message.ReplyElement{
			ReplySeq: int32(messageSeq),
			Sender:   sender,
			Time:     int32(msgTime),
			Elements: bot.ConvertStringMessage(spec, customText, sourceType),
		}

	case err == nil:
		org, err := db.GetMessageByGlobalID(int32(mid))
		if err != nil {
			return nil, err
		}
		re = &message.ReplyElement{
			ReplySeq: org.GetAttribute().MessageSeq,
			Sender:   org.GetAttribute().SenderUin,
			Time:     int32(org.GetAttribute().Timestamp),
			Elements: bot.ConvertContentMessage(org.GetContent(), sourceType, true),
		}

	default:
		return nil, errors.New("reply消息中必须包含 text 或 id")
	}
	return re, nil
}

func (bot *CQBot) voice(elem msg.Element) (m any, err error) {
	f := elem.Get("file")
	u := elem.Get("url")
	if f == "" && u == "" {
		return nil, errors.New("params error")
	}

	if f == "" {
		hash := md5.Sum([]byte(u))
		f = hex.EncodeToString(hash[:]) + ".cache"
	}

	if os.IsNotExist(err) && u != "" {
		cacheFile := path.Join(global.VoicePath, f)
		_, err = os.Stat(cacheFile)
		if err != nil {
			log.Warnf("语音文件 %v 访问失败: %v", u, err)
		}
		err = download.Request{URL: u}.WriteToFile(cacheFile)
		if err != nil {
			log.Warnf("语音文件 %v 下载失败: %v", u, err)
		}
	}

	data, err := global.FindFile(f, elem.Get("cache"), global.VoicePath)
	voiceTime, _ := strconv.ParseInt(elem.Get("during"), 10, 32)
	if err != nil {
		return nil, err
	}
	if !global.IsAMRorSILK(data) {
		mt, ok := mime.CheckAudio(bytes.NewReader(data))
		if !ok {
			return nil, errors.New("voice type error: " + mt)
		}
		data, err = global.EncoderSilk(data)
		if err != nil {
			return nil, err
		}
	}
	if voiceTime == 0 {
		voiceTime, err = global.GetSilkFileDuration(bytes.NewReader(data), 20)
		if err != nil {
			log.Warnf("语音文件 %v 无法分析它的真实语音长度: %v", u, err)
		}
	}
	return &msg.LocalVoice{Data: data, During: int32(voiceTime)}, nil
}

func (bot *CQBot) at(id, name string) (m any, err error) {
	t, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return nil, err
	}
	name = strings.TrimSpace(name)
	if len(name) > 0 {
		name = "@" + name
	}
	return message.NewAt(t, name), nil
}

// convertV11 ConvertElement11
func (bot *CQBot) convertV11(elem msg.Element) (m any, ok bool, err error) {
	switch elem.Type {
	default:
		// not ok
		return
	case "at":
		qq := elem.Get("qq")
		if qq == "" {
			qq = elem.Get("target")
		}
		if qq == "all" {
			m = message.AtAll()
			break
		}
		m, err = bot.at(qq, elem.Get("name"))
	case "record":
		m, err = bot.voice(elem)
	}
	ok = true
	return
}

// convertV12 ConvertElement12
func (bot *CQBot) convertV12(elem msg.Element) (m any, ok bool, err error) {
	switch elem.Type {
	default:
		// not ok
		return
	case "mention":
		m, err = bot.at(elem.Get("user_id"), elem.Get("name"))
	case "mention_all":
		m = message.AtAll()
	case "voice":
		m, err = bot.voice(elem)
	}
	ok = true
	return
}

// ConvertElement 将解码后的消息转换为MiraiGoElement.
//
// 返回 interface{} 存在三种类型
//
// message.IMessageElement []message.IMessageElement nil
func (bot *CQBot) ConvertElement(spec *onebot.Spec, elem msg.Element, sourceType message.SourceType) (m any, err error) {
	var ok bool
	switch spec.Version {
	case 11:
		m, ok, err = bot.convertV11(elem)
	case 12:
		m, ok, err = bot.convertV12(elem)
	default:
		panic("invalid onebot version:" + strconv.Itoa(spec.Version))
	}
	if ok {
		return m, err
	}

	switch elem.Type {
	case "text":
		text := elem.Get("text")
		if base.SplitURL {
			var ret []message.IMessageElement
			for _, text := range param.SplitURL(text) {
				ret = append(ret, message.NewText(text))
			}
			return ret, nil
		}
		return message.NewText(text), nil
	case "image":
		img, err := bot.makeImageOrVideoElem(elem, sourceType)
		if err != nil {
			return nil, err
		}
		tp := elem.Get("type")
		flash, id := false, int64(0)
		switch tp {
		case "flash":
			flash = true
		case "show":
			id, _ = strconv.ParseInt(elem.Get("id"), 10, 64)
			if id < 40000 || id >= 40006 {
				id = 40000
			}
		default:
			return img, nil
		}
		if element, ok := img.(*message.NewTechImageElement); ok {
			img = element.GetLegacyOrSelf()
		}
		switch img := img.(type) {
		case *msg.LocalImage:
			img.Flash = flash
			img.EffectID = int32(id)
		case *message.GroupImageElement:
			img.EffectID = int32(id)
			i, _ := strconv.ParseInt(elem.Get("subType"), 10, 64)
			img.ImageBizType = message.ImageBizType(i)
		case *message.FriendImageElement:
			img.Flash = flash
		}
		return img, nil
	case "reply":
		return bot.reply(spec, elem, sourceType)
	case "forward":
		id := elem.Get("id")
		if id == "" {
			return nil, errors.New("forward 消息中必须包含 id")
		}
		fwdMsg := bot.Client.DownloadForwardMessage(id)
		if fwdMsg == nil {
			return nil, errors.New("forward 消息不存在或已过期")
		}
		return fwdMsg, nil

	case "poke":
		t, _ := strconv.ParseInt(elem.Get("qq"), 10, 64)
		return &msg.Poke{Target: t}, nil
	case "tts":
		data, err := bot.Client.GetTts(elem.Get("text"))
		if err != nil {
			return nil, err
		}
		return &msg.LocalVoice{Data: base.ResampleSilk(data)}, nil
	case "face":
		id, err := strconv.Atoi(elem.Get("id"))
		if err != nil {
			return nil, err
		}
		if elem.Get("type") == "sticker" {
			return &message.AnimatedSticker{ID: int32(id)}, nil
		}
		return message.NewFace(int32(id)), nil
	case "share":
		return message.NewUrlShare(elem.Get("url"), elem.Get("title"), elem.Get("content"), elem.Get("image")), nil
	case "music":
		id := elem.Get("id")
		switch elem.Get("type") {
		case "qq":
			info, err := global.QQMusicSongInfo(id)
			if err != nil {
				return nil, err
			}
			if !info.Get("track_info").Exists() {
				return nil, errors.New("song not found")
			}
			albumMid := info.Get("track_info.album.mid").String()
			pinfo, _ := download.Request{URL: "https://u.y.qq.com/cgi-bin/musicu.fcg?g_tk=2034008533&uin=0&format=json&data={\"comm\":{\"ct\":23,\"cv\":0},\"url_mid\":{\"module\":\"vkey.GetVkeyServer\",\"method\":\"CgiGetVkey\",\"param\":{\"guid\":\"4311206557\",\"songmid\":[\"" + info.Get("track_info.mid").Str + "\"],\"songtype\":[0],\"uin\":\"0\",\"loginflag\":1,\"platform\":\"23\"}}}&_=1599039471576"}.JSON()
			jumpURL := "https://i.y.qq.com/v8/playsong.html?platform=11&appshare=android_qq&appversion=10030010&hosteuin=oKnlNenz7i-s7c**&songmid=" + info.Get("track_info.mid").Str + "&type=0&appsongtype=1&_wv=1&source=qq&ADTAG=qfshare"
			content := info.Get("track_info.singer.0.name").String()
			if elem.Get("content") != "" {
				content = elem.Get("content")
			}
			return &message.MusicShareElement{
				MusicType:  message.QQMusic,
				Title:      info.Get("track_info.name").Str,
				Summary:    content,
				Url:        jumpURL,
				PictureUrl: "https://y.gtimg.cn/music/photo_new/T002R180x180M000" + albumMid + ".jpg",
				MusicUrl:   pinfo.Get("url_mid.data.midurlinfo.0.purl").String(),
			}, nil
		case "163":
			info, err := global.NeteaseMusicSongInfo(id)
			if err != nil {
				return nil, err
			}
			if !info.Exists() {
				return nil, errors.New("song not found")
			}
			artistName := ""
			if info.Get("artists.0").Exists() {
				artistName = info.Get("artists.0.name").String()
			}
			return &message.MusicShareElement{
				MusicType:  message.CloudMusic,
				Title:      info.Get("name").String(),
				Summary:    artistName,
				Url:        "https://music.163.com/song/?id=" + id,
				PictureUrl: info.Get("album.picUrl").String(),
				MusicUrl:   "https://music.163.com/song/media/outer/url?id=" + id,
			}, nil
		case "custom":
			if elem.Get("subtype") != "" {
				var subType int
				switch elem.Get("subtype") {
				default:
					subType = message.QQMusic
				case "163":
					subType = message.CloudMusic
				case "migu":
					subType = message.MiguMusic
				case "kugou":
					subType = message.KugouMusic
				case "kuwo":
					subType = message.KuwoMusic
				}
				return &message.MusicShareElement{
					MusicType:  subType,
					Title:      elem.Get("title"),
					Summary:    elem.Get("content"),
					Url:        elem.Get("url"),
					PictureUrl: elem.Get("image"),
					MusicUrl:   elem.Get("voice"),
				}, nil
			}
			xml := fmt.Sprintf(`<?xml version='1.0' encoding='UTF-8' standalone='yes' ?><msg serviceID="2" templateID="1" action="web" brief="[分享] %s" sourceMsgId="0" url="%s" flag="0" adverSign="0" multiMsgFlag="0"><item layout="2"><voice cover="%s" src="%s"/><title>%s</title><summary>%s</summary></item><source name="音乐" icon="https://i.gtimg.cn/open/app_icon/01/07/98/56/1101079856_100_m.png" url="http://web.p.qq.com/qqmpmobile/aio/app.html?id=1101079856" action="app" a_actionData="com.tencent.qqmusic" i_actionData="tencent1101079856://" appid="1101079856" /></msg>`,
				utils.XmlEscape(elem.Get("title")), elem.Get("url"), elem.Get("image"), elem.Get("voice"), utils.XmlEscape(elem.Get("title")), utils.XmlEscape(elem.Get("content")))
			return &message.ServiceElement{
				Id:      60,
				Content: xml,
				SubType: "music",
			}, nil
		}
		return nil, errors.New("unsupported music type: " + elem.Get("type"))
	case "dice":
		value := elem.Get("value")
		i, _ := strconv.ParseInt(value, 10, 64)
		if i < 0 || i > 6 {
			return nil, errors.New("invalid dice value " + value)
		}
		return message.NewDice(int32(i)), nil
	case "rps":
		value := elem.Get("value")
		i, _ := strconv.ParseInt(value, 10, 64)
		if i < 0 || i > 2 {
			return nil, errors.New("invalid finger-guessing value " + value)
		}
		return message.NewFingerGuessing(int32(i)), nil
	case "xml":
		resID := elem.Get("resid")
		template := elem.Get("data")
		i, _ := strconv.ParseInt(resID, 10, 64)
		m := message.NewRichXml(template, i)
		return m, nil
	case "json":
		resID := elem.Get("resid")
		data := elem.Get("data")
		i, _ := strconv.ParseInt(resID, 10, 64)
		if i == 0 {
			// 默认情况下走小程序通道
			return message.NewLightApp(data), nil
		}
		// resid不为0的情况下走富文本通道，后续补全透传service Id，此处暂时不处理 TODO
		return message.NewRichJson(data), nil
	case "cardimage":
		source := elem.Get("source")
		icon := elem.Get("icon")
		brief := elem.Get("brief")
		parseIntWithDefault := func(name string, origin int64) int64 {
			v, _ := strconv.ParseInt(elem.Get(name), 10, 64)
			if v <= 0 {
				return origin
			}
			return v
		}
		minWidth := parseIntWithDefault("minwidth", 200)
		maxWidth := parseIntWithDefault("maxwidth", 500)
		minHeight := parseIntWithDefault("minheight", 200)
		maxHeight := parseIntWithDefault("maxheight", 1000)
		img, err := bot.makeImageOrVideoElem(elem, sourceType)
		if err != nil {
			return nil, errors.New("send cardimage faild")
		}
		return bot.makeShowPic(img, source, brief, icon, minWidth, minHeight, maxWidth, maxHeight, sourceType == message.SourceGroup)
	case "video":
		file, err := bot.makeImageOrVideoElem(elem, sourceType)
		if err != nil {
			return nil, err
		}
		v, ok := file.(*msg.LocalVideo)
		if !ok {
			return file, nil
		}
		if v.File == "" {
			return v, nil
		}
		var data []byte
		if cover := elem.Get("cover"); cover != "" {
			data, _ = global.FindFile(cover, elem.Get("cache"), global.ImagePath)
		} else {
			err = global.ExtractCover(v.File, v.File+".jpg")
			if err != nil {
				return nil, err
			}
			data, _ = os.ReadFile(v.File + ".jpg")
		}
		v.Thumb = bytes.NewReader(data)
		video, _ := os.Open(v.File)
		defer video.Close()
		_, _ = video.Seek(4, io.SeekStart)
		header := make([]byte, 4)
		_, _ = video.Read(header)
		if !bytes.Equal(header, []byte{0x66, 0x74, 0x79, 0x70}) { // check file header ftyp
			_, _ = video.Seek(0, io.SeekStart)
			hash, _ := utils.ComputeMd5AndLength(video)
			cacheFile := path.Join(global.CachePath, hex.EncodeToString(hash)+".mp4")
			if !(elem.Get("cache") == "" || elem.Get("cache") == "1") || !global.PathExists(cacheFile) {
				err = global.EncodeMP4(v.File, cacheFile)
				if err != nil {
					return nil, err
				}
			}
			v.File = cacheFile
		}
		return v, nil
	case "file":
		path := elem.Get("path")
		name := elem.Get("name")
		size, _ := strconv.ParseInt(elem.Get("size"), 10, 64)
		busid, _ := strconv.ParseInt(elem.Get("busid"), 10, 64)
		return &message.GroupFileElement{
			Name:  name,
			Size:  size,
			Path:  path,
			Busid: int32(busid),
		}, nil
	default:
		return nil, errors.New("unsupported message type: " + elem.Type)
	}
}

// makeImageOrVideoElem 图片 elem 生成器，单独拎出来，用于公用
func (bot *CQBot) makeImageOrVideoElem(elem msg.Element, sourceType message.SourceType) (message.IMessageElement, error) {
	f := elem.Get("file")
	u := elem.Get("url")
	video := elem.Type == "video"
	if u == "" {
		if strings.HasPrefix(f, "http") {
			u = f
		}
	}
	if u != "" {
		var hash [16]byte
		if f == "" {
			hash = md5.Sum([]byte(u))
		} else {
			hash = md5.Sum([]byte(f))
		}
		cacheFile := path.Join(global.CachePath, hex.EncodeToString(hash[:])+".cache")
		maxSize := int64(maxImageSize)
		if video {
			maxSize = maxVideoSize
		}
		thread, _ := strconv.Atoi(elem.Get("c"))
		useCacheFile := false
		if elem.Get("cache") == "" || elem.Get("cache") == "1" {
			useCacheFile = true
		} else {
			_ = os.Remove(cacheFile)
		}
		exist := global.PathExists(cacheFile)
		if !exist || !useCacheFile {
			var requestURL = u
			if strings.HasPrefix(requestURL, "https://multimedia.nt.qq.com.cn/") {
				//因为rkey有效期非常短，因此要在这里请求之前再次处理一下rkey（尽管前面可能某一处的调用给的url的rkey是最新的）
				parsedURL, _ := url.Parse(requestURL)
				queryParams := parsedURL.Query()
				queryParams.Del("rkey")
				parsedURL.RawQuery = queryParams.Encode()
				requestURL = parsedURL.String()
				if sourceType == message.SourceGroup {
					requestURL += bot.Client.GetRKeyString(nt.BusinessGroupImage)
				} else {
					requestURL += bot.Client.GetRKeyString(nt.BusinessFriendImage)
				}
			}
			r := download.Request{URL: requestURL, Limit: maxSize}
			if useCacheFile {
				//cache = 1 写盘
				if err := r.WriteToFileMultiThreading(cacheFile, thread); err != nil {
					return nil, err
				}
			} else {
				//case = 0 写内存
				stream, err := r.DownloadReadSeeker()
				if err == nil {
					return &msg.LocalImage{Stream: stream}, nil
				}
				return nil, err
			}
		}
		if video {
			return &msg.LocalVideo{File: cacheFile}, nil
		}
		return &msg.LocalImage{File: cacheFile, URL: u}, nil
	}

	if strings.HasPrefix(f, "file") {
		fu, err := url.Parse(f)
		if err != nil {
			return nil, err
		}
		if runtime.GOOS == `windows` && strings.HasPrefix(fu.Path, "/") {
			fu.Path = fu.Path[1:]
		}
		info, err := os.Stat(fu.Path)
		if err != nil {
			if !os.IsExist(err) {
				return nil, errors.New("file not found")
			}
			return nil, err
		}
		if video {
			if info.Size() == 0 || info.Size() >= maxVideoSize {
				return nil, errors.New("invalid video size")
			}
			return &msg.LocalVideo{File: fu.Path}, nil
		}
		if info.Size() == 0 || info.Size() >= maxImageSize {
			return nil, errors.New("invalid image size")
		}
		return &msg.LocalImage{File: fu.Path, URL: f}, nil
	}
	if !video && strings.HasPrefix(f, "base64") {
		b, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(f, "base64://"))
		if err != nil {
			return nil, err
		}
		return &msg.LocalImage{Stream: bytes.NewReader(b), URL: f}, nil
	}
	if !video && strings.HasPrefix(f, "base16384") {
		b, err := b14.UTF82UTF16BE(utils.S2B(strings.TrimPrefix(f, "base16384://")))
		if err != nil {
			return nil, err
		}
		return &msg.LocalImage{Stream: bytes.NewReader(b14.Decode(b)), URL: f}, nil
	}
	if video {
		if strings.HasSuffix(f, ".video") {
			hash, err := hex.DecodeString(strings.TrimSuffix(f, ".video"))
			if err == nil {
				if b := cache.Media.Get(hash); b != nil {
					return bot.readVideoCache(b), nil
				}
			}
		}
		rawPath := path.Join(global.VideoPath, f)
		if !global.PathExists(rawPath) {
			return nil, errors.New("invalid video")
		}
		return &msg.LocalVideo{File: rawPath}, nil
	}
	if strings.HasSuffix(f, ".image") {
		hash, err := hex.DecodeString(strings.TrimSuffix(f, ".image"))
		if err == nil {
			if b := cache.Media.Get(hash); b != nil {
				return bot.readImageCache(elem, b, sourceType)
			}
		}
	}
	rawPath := path.Join(global.ImagePath, f)
	if global.PathExists(rawPath) && f != "" {
		return &msg.LocalImage{File: rawPath, URL: u}, nil
	}
	return nil, errors.New("invalid image")
}

func (bot *CQBot) readImageCache(elem msg.Element, record *database.DatabaseRecord, sourceType message.SourceType) (message.IMessageElement, error) {
	var err error
	var rsp message.IMessageElement
	if record.Image == nil {
		return nil, errors.New("invalid cache")
	}
	hash := record.Image.Md5
	size := int32(record.Image.Size)
	imageURL := bot.Client.GetDatabaseImageUrl(record.Image)
	if imageURL != "" {
		elem.Type = "image"
		elem.Data = append(elem.Data, msg.Pair{K: "url", V: imageURL})
		rsp, err = bot.makeImageOrVideoElem(elem, sourceType)
		if err == nil {
			return rsp, nil
		}
	}
	switch sourceType { // nolint:exhaustive
	case message.SourceGroup:
		rsp, err = bot.Client.QueryImage(bot.Client.GroupList[0].Code, 0, record.Image)
	case message.SourceGuildChannel:
		if len(bot.Client.GuildService.Guilds) == 0 {
			err = errors.New("cannot query guild image: not any joined guild")
			break
		}
		guild := bot.Client.GuildService.Guilds[0]
		rsp, err = bot.Client.GuildService.QueryImage(guild.GuildId, guild.Channels[0].ChannelId, hash, uint64(size))
	default:
		rsp, err = bot.Client.QueryImage(0, bot.Client.Uin, record.Image)
	}
	return rsp, err
}

func (bot *CQBot) readVideoCache(record *database.DatabaseRecord) message.IMessageElement {
	return &message.ShortVideoElement{ // todo 检查缓存是否有效
		Md5:       record.Video.Md5,
		ThumbMd5:  record.Video.ThumbMd5,
		Size:      int32(record.Video.Size),
		ThumbSize: int32(record.Video.ThumbSize),
		Name:      record.Video.Name,
		Uuid:      record.Video.Uuid,
	}
}

// makeShowPic 一种xml 方式发送的群消息图片
func (bot *CQBot) makeShowPic(elem message.IMessageElement, source string, brief string, icon string, minWidth int64, minHeight int64, maxWidth int64, maxHeight int64, group bool) ([]message.IMessageElement, error) {
	xml := ""
	var suf message.IMessageElement
	if brief == "" {
		brief = "&#91;分享&#93;我看到一张很赞的图片，分享给你，快来看！"
	}
	if local, ok := elem.(*msg.LocalImage); ok {
		r := rand.Uint32()
		typ := message.SourceGroup
		if !group {
			typ = message.SourcePrivate
		}
		e, err := bot.uploadLocalImage(message.Source{SourceType: typ, PrimaryID: int64(r)}, local)
		if err != nil {
			log.Warnf("警告: 图片上传失败: %v", err)
			return nil, err
		}
		elem = e
	}
	if element, ok := elem.(*message.NewTechImageElement); ok {
		elem = element.GetLegacyOrSelf()
	}
	switch i := elem.(type) {
	case *message.GroupImageElement:
		xml = fmt.Sprintf(`<?xml version='1.0' encoding='UTF-8' standalone='yes' ?><msg serviceID="5" templateID="12345" action="" brief="%s" sourceMsgId="0" url="%s" flag="0" adverSign="0" multiMsgFlag="0"><item layout="0" advertiser_id="0" aid="0"><image uuid="%x" md5="%x" GroupFiledid="0" filesize="%d" local_path="%s" minWidth="%d" minHeight="%d" maxWidth="%d" maxHeight="%d" /></item><source name="%s" icon="%s" action="" appid="-1" /></msg>`, brief, "", i.Md5, i.Md5, 0, "", minWidth, minHeight, maxWidth, maxHeight, source, icon)
		suf = i
	case *message.FriendImageElement:
		xml = fmt.Sprintf(`<?xml version='1.0' encoding='UTF-8' standalone='yes' ?><msg serviceID="5" templateID="12345" action="" brief="%s" sourceMsgId="0" url="%s" flag="0" adverSign="0" multiMsgFlag="0"><item layout="0" advertiser_id="0" aid="0"><image uuid="%x" md5="%x" GroupFiledid="0" filesize="%d" local_path="%s" minWidth="%d" minHeight="%d" maxWidth="%d" maxHeight="%d" /></item><source name="%s" icon="%s" action="" appid="-1" /></msg>`, brief, "", i.Md5, i.Md5, 0, "", minWidth, minHeight, maxWidth, maxHeight, source, icon)
		suf = i
	case *message.NewTechImageElement:
		xml = fmt.Sprintf(`<?xml version='1.0' encoding='UTF-8' standalone='yes' ?><msg serviceID="5" templateID="12345" action="" brief="%s" sourceMsgId="0" url="%s" flag="0" adverSign="0" multiMsgFlag="0"><item layout="0" advertiser_id="0" aid="0"><image uuid="%x" md5="%x" GroupFiledid="0" filesize="%d" local_path="%s" minWidth="%d" minHeight="%d" maxWidth="%d" maxHeight="%d" /></item><source name="%s" icon="%s" action="" appid="-1" /></msg>`, brief, "", i.Md5, i.Md5, 0, "", minWidth, minHeight, maxWidth, maxHeight, source, icon)
		suf = i
	}
	if xml == "" {
		return nil, errors.New("生成xml图片消息失败")
	}
	ret := []message.IMessageElement{suf, message.NewRichXml(xml, 5)}
	return ret, nil
}
