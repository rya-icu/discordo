package cmd

import (
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/ayn2op/discordo/internal/config"
	"github.com/ayn2op/discordo/internal/ui"
	"github.com/ayn2op/tview"
	"github.com/diamondburned/arikawa/v3/discord"
	"github.com/diamondburned/arikawa/v3/gateway"
	"github.com/diamondburned/ningen/v3"
	"github.com/gdamore/tcell/v2"
	"golang.design/x/clipboard"
)

// 用于存储节点额外信息的结构体
type nodeInfo struct {
	isOriginal     bool
	originalParent *tview.TreeNode
}

type guildsTree struct {
	*tview.TreeView
	cfg               *config.Config
	selectedChannelID discord.ChannelID
	selectedGuildID   discord.GuildID

	filterQuery string
	filteredNodes []*tview.TreeNode
	isFiltering   bool
	originalRoot  *tview.TreeNode // 保存原始根节点
	tempRoot      *tview.TreeNode // 过滤模式下的临时根节点
	resultNode    *tview.TreeNode // 检索结果临时节点
}

// 辅助函数：查找节点在切片中的索引
func (gt *guildsTree) findNodeIndex(nodes []*tview.TreeNode, target *tview.TreeNode) int {
    for i, node := range nodes {
        if node == target {
            return i
        }
    }
    return -1
}

// 辅助函数：检查节点是否在切片中
func (gt *guildsTree) containsNode(nodes []*tview.TreeNode, target *tview.TreeNode) bool {
    return gt.findNodeIndex(nodes, target) != -1
}

func newGuildsTree(cfg *config.Config) *guildsTree {
	// 创建原始根节点
	originalRoot := tview.NewTreeNode("Servers")
	// 存储原始节点信息
	originalRoot.SetReference(nodeInfo{isOriginal: true})

	// 创建过滤结果临时节点
	resultNode := tview.NewTreeNode("Current Results")
	resultNode.SetReference(nodeInfo{isOriginal: false})
	
	// 创建过滤模式下的临时根节点
	tempRoot := tview.NewTreeNode("Filter Root")
	tempRoot.SetReference(nodeInfo{isOriginal: false})
	tempRoot.AddChild(resultNode)

	gt := &guildsTree{
		TreeView: tview.NewTreeView(),
		cfg:      cfg,

		originalRoot:  originalRoot,
		tempRoot:      tempRoot,
		resultNode:    resultNode,

	}


	gt.Box = ui.ConfigureBox(gt.Box, &cfg.Theme)
	gt.
		SetRoot(originalRoot).
		SetTopLevel(1).
		SetGraphics(cfg.Theme.GuildsTree.Graphics).
		SetGraphicsColor(tcell.GetColor(cfg.Theme.GuildsTree.GraphicsColor)).
		SetSelectedFunc(gt.onSelected).
		SetTitle("Guilds").
		SetInputCapture(gt.onInputCapture)

	return gt
}


func (gt *guildsTree) createFolderNode(folder gateway.GuildFolder) {
	name := "Folder"
	if folder.Name != "" {
		name = fmt.Sprintf("[%s]%s[-]", folder.Color, folder.Name)
	}

	folderNode := tview.NewTreeNode(name).SetExpanded(gt.cfg.Theme.GuildsTree.AutoExpandFolders)
	gt.GetRoot().AddChild(folderNode)

	for _, gID := range folder.GuildIDs {
		guild, err := discordState.Cabinet.Guild(gID)
		if err != nil {
			slog.Error("failed to get guild from state", "guild_id", gID, "err", err)
			continue
		}

		gt.createGuildNode(folderNode, *guild)
	}
}

func (gt *guildsTree) unreadStyle(indication ningen.UnreadIndication) tcell.Style {
	var style tcell.Style
	switch indication {
	case ningen.ChannelRead:
		style = style.Dim(true)
	case ningen.ChannelMentioned:
		style = style.Underline(true)
		fallthrough
	case ningen.ChannelUnread:
		style = style.Bold(true)
	}

	return style
}

func (gt *guildsTree) getGuildNodeStyle(guildID discord.GuildID) tcell.Style {
	indication := discordState.GuildIsUnread(guildID, ningen.GuildUnreadOpts{UnreadOpts: ningen.UnreadOpts{IncludeMutedCategories: true}})
	return gt.unreadStyle(indication)
}

func (gt *guildsTree) getChannelNodeStyle(channelID discord.ChannelID) tcell.Style {
	indication := discordState.ChannelIsUnread(channelID, ningen.UnreadOpts{IncludeMutedCategories: true})
	return gt.unreadStyle(indication)
}

func (gt *guildsTree) createGuildNode(n *tview.TreeNode, guild discord.Guild) {
	guildNode := tview.NewTreeNode(guild.Name).
		SetReference(guild.ID).
		SetTextStyle(gt.getGuildNodeStyle(guild.ID))
	n.AddChild(guildNode)
}

func (gt *guildsTree) createChannelNode(node *tview.TreeNode, channel discord.Channel) {
	if channel.Type != discord.DirectMessage && channel.Type != discord.GroupDM && !discordState.HasPermissions(channel.ID, discord.PermissionViewChannel) {
		return
	}

	channelNode := tview.NewTreeNode(ui.ChannelToString(channel)).
		SetReference(channel.ID).
		SetTextStyle(gt.getChannelNodeStyle(channel.ID))
	node.AddChild(channelNode)
}

func (gt *guildsTree) createChannelNodes(node *tview.TreeNode, channels []discord.Channel) {
	for _, channel := range channels {
		if channel.Type != discord.GuildCategory && !channel.ParentID.IsValid() {
			gt.createChannelNode(node, channel)
		}
	}

PARENT_CHANNELS:
	for _, channel := range channels {
		if channel.Type == discord.GuildCategory {
			for _, nested := range channels {
				if nested.ParentID == channel.ID {
					gt.createChannelNode(node, channel)
					continue PARENT_CHANNELS
				}
			}
		}
	}

	for _, channel := range channels {
		if channel.ParentID.IsValid() {
			var parent *tview.TreeNode
			node.Walk(func(node, _ *tview.TreeNode) bool {
				if node.GetReference() == channel.ParentID {
					parent = node
					return false
				}

				return true
			})

			if parent != nil {
				gt.createChannelNode(parent, channel)
			}
		}
	}
}

func (gt *guildsTree) onSelected(node *tview.TreeNode) {
	app.messageInput.reset()

	if len(node.GetChildren()) != 0 {
		node.SetExpanded(!node.IsExpanded())
		return
	}

	switch ref := node.GetReference().(type) {
	case discord.GuildID:
		go discordState.MemberState.Subscribe(ref)

		channels, err := discordState.Cabinet.Channels(ref)
		if err != nil {
			slog.Error("failed to get channels", "err", err, "guild_id", ref)
			return
		}

		sort.Slice(channels, func(i, j int) bool {
			return channels[i].Position < channels[j].Position
		})

		gt.createChannelNodes(node, channels)
	case discord.ChannelID:
		channel, err := discordState.Cabinet.Channel(ref)
		if err != nil {
			slog.Error("failed to get channel", "channel_id", ref)
			return
		}

		go discordState.ReadState.MarkRead(channel.ID, channel.LastMessageID)

		messages, err := discordState.Messages(channel.ID, uint(gt.cfg.MessagesLimit))
		if err != nil {
			slog.Error("failed to get messages", "err", err, "channel_id", channel.ID, "limit", gt.cfg.MessagesLimit)
			return
		}

		if guildID := channel.GuildID; guildID.IsValid() {
			app.messagesList.requestGuildMembers(guildID, messages)
		}

		app.messagesList.reset()
		app.messagesList.setTitle(*channel)
		app.messagesList.drawMessages(messages)
		app.messagesList.ScrollToEnd()

		hasPerm := channel.Type != discord.DirectMessage && channel.Type != discord.GroupDM && !discordState.HasPermissions(channel.ID, discord.PermissionSendMessages)
		app.messageInput.SetDisabled(hasPerm)

		gt.selectedChannelID = channel.ID
		gt.selectedGuildID = channel.GuildID
		app.SetFocus(app.messageInput)
	case nil: // Direct messages
		channels, err := discordState.PrivateChannels()
		if err != nil {
			slog.Error("failed to get private channels", "err", err)
			return
		}

		sort.Slice(channels, func(a, b int) bool {
			msgID := func(ch discord.Channel) discord.MessageID {
				if ch.LastMessageID.IsValid() {
					return ch.LastMessageID
				}
				return discord.MessageID(ch.ID)
			}
			return msgID(channels[a]) > msgID(channels[b])
		})

		for _, c := range channels {
			gt.createChannelNode(node, c)
		}
	}
}

func (gt *guildsTree) collapseParentNode(node *tview.TreeNode) {
	gt.
		GetRoot().
		Walk(func(n, parent *tview.TreeNode) bool {
			if n == node && parent.GetLevel() != 0 {
				parent.Collapse()
				gt.SetCurrentNode(parent)
				return false
			}

			return true
		})
}


// 切换过滤模式
func (gt *guildsTree) toggleFilterMode() {
	if gt.isFiltering {
		gt.clearFilter()
	} else {
		gt.enterFilterMode()
	}
}

// 进入过滤模式
func (gt *guildsTree) enterFilterMode() {
	gt.isFiltering = true
	gt.filterQuery = ""
	// 切换到临时根节点
	gt.SetRoot(gt.tempRoot)
	// 清空之前的结果
	gt.clearResultNode()
	gt.SetTitle("Filtering (ESC to cancel)")
	app.SetFocus(gt)
	app.Draw()
}

// 清除过滤状态
func (gt *guildsTree) clearFilter() {
	gt.isFiltering = false
	gt.filterQuery = ""
	// 切换回原始根节点
	gt.SetRoot(gt.originalRoot)
	// 清空结果节点
	gt.clearResultNode()
	gt.SetTitle("")

	app.SetFocus(gt)
	app.Draw()
}

// 清空结果节点的子节点
func (gt *guildsTree) clearResultNode() {
	// 移除所有子节点
	for _, child := range gt.resultNode.GetChildren() {
		gt.resultNode.RemoveChild(child)
	}
	gt.filteredNodes = []*tview.TreeNode{}
}

// 应用过滤并在临时节点中显示结果
func (gt *guildsTree) applyFilter() {
	// 先清空现有结果
	gt.clearResultNode()
	
	if gt.filterQuery == "" {
		return
	}

	query := strings.ToLower(gt.filterQuery)
	
	// 遍历所有原始节点查找匹配项
	gt.originalRoot.Walk(func(node, parent *tview.TreeNode) bool {
		// 检查是否为原始节点
		if info, ok := node.GetReference().(nodeInfo); !ok || !info.isOriginal {
			return true
		}
		
		// 只考虑频道节点
		if _, ok := node.GetReference().(discord.ChannelID); ok {
			text := strings.ToLower(node.GetText())
			if strings.Contains(text, query) {
				// 创建克隆节点用于显示在结果中
				clone := tview.NewTreeNode(node.GetText()).
					SetReference(node.GetReference()). // 保留原始引用
					SetColor(tcell.ColorGreen)
				
				// 添加到结果节点
				gt.resultNode.AddChild(clone)
				gt.filteredNodes = append(gt.filteredNodes, clone)
			}
		}
		return true
	})
	
	// 如果有匹配项，选中第一个
	if len(gt.filteredNodes) > 0 {
		gt.SetCurrentNode(gt.filteredNodes[0])
		// 展开结果节点
		gt.resultNode.SetExpanded(true)
	}
	
	app.SetFocus(gt)
	app.Draw()
}

// 进入选中的频道
func (gt *guildsTree) enterSelectedChannel() {
	currentNode := gt.GetCurrentNode()
	if currentNode != nil {
		gt.onSelected(currentNode)
	}
}

func (gt *guildsTree) onInputCapture(event *tcell.EventKey) *tcell.EventKey {

	if event.Key() == tcell.KeyCtrlF {
		gt.toggleFilterMode()
		return nil
	}

// 如果处于过滤状态，处理过滤输入
	if gt.isFiltering {
		switch event.Key() {
		case tcell.KeyEnter:
			// 回车进入选中的频道
			gt.enterSelectedChannel()
			gt.clearFilter()
			return nil
		case tcell.KeyEscape:
			// ESC取消过滤
			gt.clearFilter()
			return nil
		case tcell.KeyBackspace, tcell.KeyBackspace2:
			// 退格键处理
			if len(gt.filterQuery) > 0 {
				gt.filterQuery = gt.filterQuery[:len(gt.filterQuery)-1]
				gt.applyFilter()
			} else {
				// 如果已经没有字符了，退出过滤模式
				gt.clearFilter()
			}
			return nil
		case tcell.KeyUp, tcell.KeyDown, tcell.KeyPgUp, tcell.KeyPgDn:
			// 导航键正常处理
			return event
		}

		// 处理可打印字符
		if event.Rune() != 0 && event.Rune() >= 32 {
			gt.filterQuery += string(event.Rune())
			gt.applyFilter()
			return nil
		}
	}

	switch event.Name() {
	case gt.cfg.Keys.GuildsTree.CollapseParentNode:
		gt.collapseParentNode(gt.GetCurrentNode())
		return nil
	case gt.cfg.Keys.GuildsTree.MoveToParentNode:
		return tcell.NewEventKey(tcell.KeyRune, 'K', tcell.ModNone)

	case gt.cfg.Keys.GuildsTree.SelectPrevious:
		return tcell.NewEventKey(tcell.KeyUp, 0, tcell.ModNone)
	case gt.cfg.Keys.GuildsTree.SelectNext:
		return tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone)
	case gt.cfg.Keys.GuildsTree.SelectFirst:
		gt.Move(gt.GetRowCount() * -1)
		// return tcell.NewEventKey(tcell.KeyHome, 0, tcell.ModNone)
	case gt.cfg.Keys.GuildsTree.SelectLast:
		gt.Move(gt.GetRowCount())
		// return tcell.NewEventKey(tcell.KeyEnd, 0, tcell.ModNone)

	case gt.cfg.Keys.GuildsTree.SelectCurrent:
		return tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone)

	case gt.cfg.Keys.GuildsTree.YankID:
		gt.yankID()
	}

	return nil
}

func (gt *guildsTree) yankID() {
	node := gt.GetCurrentNode()
	if node == nil {
		return
	}

	// Reference of a tree node in the guilds tree is its ID.
	// discord.Snowflake (discord.GuildID and discord.ChannelID) have the String method.
	if id, ok := node.GetReference().(fmt.Stringer); ok {
		go clipboard.Write(clipboard.FmtText, []byte(id.String()))
	}
}
