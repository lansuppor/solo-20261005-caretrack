package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// 离线资产迁移包：把所选资产及其全部关联数据连同每条附件索引指向的资料
// 文件内容打包为单个 .zip 迁移包（export），日后可在尚不存在的新数据目录
// 整包还原（restore），还原后资料为目标目录内的独立副本，脱离源目录。
//
// 设计约束（与全工具一致）：仅用 Go 标准库；单进程顺序调用；不依赖任何
// 外部服务；源台账与原资料只读；失败不留成品包或目标目录，输入字节保持，
// 恢复条件后可用原包重试。
//
// 包内布局（全部为普通文件成员，不允许目录、链接，不允许越界成员名）：
//
//	caretrack.json          所选数据的台账（storeData 的 JSON，与日常保存同构）
//	files/f000001 ...       资料文件内容，每条附件索引一个独立成员
//	manifest.json           清单：包格式版本、台账与每份资料的大小及 SHA-256
//
// 同一路径的多条附件索引各占一个成员，保持独立；已撤销索引的资料同样打包。
// 资料成员名是包内生成名，与资料原路径无关；原绝对路径只存在于台账记录中，
// 还原时不会按它访问外部任何文件。
const (
	migrateMagic        = "caretrack-migration-package"
	migrateVersion      = 1
	migrateManifestName = "manifest.json"
	migrateLedgerName   = dataFileName
	migrateFilesDir     = "files"
	// restoreAttachDir 是还原后目标数据目录内资料副本所在的子目录。
	restoreAttachDir = "attachments"
)

// migrateFileMeta 描述包内一个资料成员与一条附件索引的映射及内容校验值。
type migrateFileMeta struct {
	AttachmentID string `json:"attachment_id"`
	Member       string `json:"member"`
	Size         int64  `json:"size"`
	SHA256       string `json:"sha256"`
}

// migrateManifest 为迁移包清单：台账与每份资料的内容校验值，以及汇总数量。
type migrateManifest struct {
	Format          string            `json:"format"`
	Version         int               `json:"version"`
	Ledger          string            `json:"ledger"`
	LedgerSize      int64             `json:"ledger_size"`
	LedgerSHA256    string            `json:"ledger_sha256"`
	Files           []migrateFileMeta `json:"files"`
	AssetCount      int               `json:"asset_count"`
	AttachmentCount int               `json:"attachment_count"`
}

// exportOutcome 为一次成功打包的结果摘要。
type exportOutcome struct {
	packagePath string
	assetIDs    []string
	attachments int
}

// restoreOutcome 为一次成功还原的结果摘要。
type restoreOutcome struct {
	targetDir   string
	assets      int
	attachments int
}

// fileMemberName 生成第 i（从 1 起）个资料成员的包内名称。
func fileMemberName(i int) string {
	return fmt.Sprintf("%s/f%06d", migrateFilesDir, i)
}

var fileMemberPattern = regexp.MustCompile(`^files/f[0-9]+$`)

// sha256Hex 返回数据的 SHA-256 小写十六进制摘要。
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// dedupeStrings 按首次出现顺序去重。
func dedupeStrings(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// selectLedgerData 从源台账中抽取所选资产及其全部工单、请求绑定、履历、
// 保养计划、备件领用记录与附件索引，组成一份独立的新台账数据。编号、序号、
// 计数器与全部业务内容原样保留，不做任何重编号。返回的切片均为新分配，
// 修改结果不会影响源台账。第三返回值为按登记履历序号排序的所选附件索引。
func selectLedgerData(src *store, ids []string) (*storeData, map[string]bool, []*Attachment) {
	selected := map[string]bool{}
	for _, id := range ids {
		selected[id] = true
	}
	d := newStoreData()
	for _, a := range src.data.Assets {
		if selected[a.ID] {
			na := *a
			d.Assets = append(d.Assets, &na)
		}
	}
	ticketIDs := map[string]bool{}
	for _, t := range src.data.Tickets {
		if selected[t.AssetID] {
			nt := *t
			d.Tickets = append(d.Tickets, &nt)
			ticketIDs[t.ID] = true
		}
	}
	for _, r := range src.data.Requests {
		if ticketIDs[r.TicketID] {
			d.Requests = append(d.Requests, r)
		}
	}
	for _, e := range src.data.Events {
		if selected[e.AssetID] {
			d.Events = append(d.Events, e)
		}
	}
	for _, p := range src.data.Plans {
		if selected[p.AssetID] {
			np := *p
			d.Plans = append(d.Plans, &np)
		}
	}
	for _, p := range src.data.Parts {
		if ticketIDs[p.TicketID] {
			np := *p
			d.Parts = append(d.Parts, &np)
		}
	}
	for _, a := range src.data.Attachments {
		if ticketIDs[a.TicketID] {
			na := *a
			d.Attachments = append(d.Attachments, &na)
		}
	}
	// 计数器原样保留：迁移包内各类编号、履历序号与计数器均与源一致。
	d.NextTicketSeq = src.data.NextTicketSeq
	d.NextPartSeq = src.data.NextPartSeq
	d.NextAttachSeq = src.data.NextAttachSeq

	regSeqs := src.attachRegSeqs()
	attachments := make([]*Attachment, len(d.Attachments))
	copy(attachments, d.Attachments)
	sort.SliceStable(attachments, func(i, j int) bool {
		return regSeqs[attachments[i].ID] < regSeqs[attachments[j].ID]
	})
	return d, ticketIDs, attachments
}

// buildMigrationZip 序列化台账并把每份资料连同清单写入 zip。资料内容由
// contents 提供（按 attachments 顺序，每条索引一项；同路径多条索引各自
// 独立）。zip 先写入 w 指向的临时文件，成功后由调用方原子改名发布。
func buildMigrationZip(w io.Writer, ledger []byte, assetCount int, attachments []*Attachment, contents [][]byte) error {
	zw := zip.NewWriter(w)
	writeMember := func(name string, data []byte) error {
		fh := &zip.FileHeader{
			Name:   name,
			Method: zip.Deflate,
		}
		fh.SetMode(0o644)
		f, err := zw.CreateHeader(fh)
		if err != nil {
			return err
		}
		if _, err := f.Write(data); err != nil {
			return err
		}
		return nil
	}
	if err := writeMember(migrateLedgerName, ledger); err != nil {
		return fmt.Errorf("写入台账成员失败: %w", err)
	}
	metas := make([]migrateFileMeta, 0, len(attachments))
	for i, a := range attachments {
		name := fileMemberName(i + 1)
		if err := writeMember(name, contents[i]); err != nil {
			return fmt.Errorf("写入资料成员失败（附件编号 %s）: %w", a.ID, err)
		}
		metas = append(metas, migrateFileMeta{
			AttachmentID: a.ID,
			Member:       name,
			Size:         int64(len(contents[i])),
			SHA256:       sha256Hex(contents[i]),
		})
	}
	manifest := migrateManifest{
		Format:          migrateMagic + "/1",
		Version:         migrateVersion,
		Ledger:          migrateLedgerName,
		LedgerSize:      int64(len(ledger)),
		LedgerSHA256:    sha256Hex(ledger),
		Files:           metas,
		AssetCount:      assetCount,
		AttachmentCount: len(metas),
	}
	var mbuf bytes.Buffer
	enc := json.NewEncoder(&mbuf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(manifest); err != nil {
		return fmt.Errorf("编码清单失败: %w", err)
	}
	if err := writeMember(migrateManifestName, mbuf.Bytes()); err != nil {
		return fmt.Errorf("写入清单成员失败: %w", err)
	}
	if err := zw.Close(); err != nil {
		return fmt.Errorf("写入迁移包失败: %w", err)
	}
	return nil
}

// exportPackage 把 dataDir 中所选资产打包到 packagePath。源台账与原资料
// 只读；包文件路径已存在、资产未知、源台账矛盾、任一资料不是可读普通文件
// 或读写失败时整次拒绝，不留下成品包，也不修改源目录任何字节。
func exportPackage(dataDir, packagePath string, assetIDs []string) (*exportOutcome, error) {
	absPkg, err := filepath.Abs(packagePath)
	if err != nil {
		return nil, fmt.Errorf("无法解析迁移包路径 %q: %w", packagePath, err)
	}
	// 已有包文件（含目录、符号链接等任何同名路径）不得覆盖。
	if _, err := os.Lstat(absPkg); err == nil {
		return nil, fmt.Errorf("迁移包文件 %s 已存在，拒绝覆盖，请另选路径", absPkg)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("无法访问迁移包路径 %s: %w", absPkg, err)
	}

	// 源台账必须已存在并通过整库一致性检查；源始终只读。
	src, err := openSourceStore(dataDir)
	if err != nil {
		return nil, err
	}
	ids := dedupeStrings(assetIDs)
	for _, id := range ids {
		if src.findAsset(id) == nil {
			return nil, fmt.Errorf("%w: 源台账中不存在资产编号 %q", errNotFound, id)
		}
	}

	d, _, attachments := selectLedgerData(src, ids)
	if err := validateData(d); err != nil {
		return nil, fmt.Errorf("所选数据的业务关联校验失败，拒绝打包: %w", err)
	}

	// 每条附件索引（含已撤销）指向的资料当前都必须是可读普通文件；同路径的
	// 多条索引逐一独立读取，各成一个成员。任一不可用则整次拒绝，不打包。
	contents := make([][]byte, len(attachments))
	for i, a := range attachments {
		if err := checkAttachFile(a.Path); err != nil {
			return nil, fmt.Errorf("附件编号 %s 指向的资料不可打包: %v", a.ID, err)
		}
		raw, rerr := os.ReadFile(a.Path)
		if rerr != nil {
			return nil, fmt.Errorf("读取附件编号 %s 的资料 %s 失败: %w", a.ID, a.Path, rerr)
		}
		contents[i] = raw
	}

	ledger, err := encodeLedger(d)
	if err != nil {
		return nil, err
	}
	assetCount := len(d.Assets)

	// 先写同目录临时文件，全部成功并 Sync 后 rename 发布；任何失败都删除
	// 临时文件，绝不在目标路径留下成品包或半成品。
	dir := filepath.Dir(absPkg)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("创建迁移包所在目录失败: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".caretrack-pkg-*.zip.tmp")
	if err != nil {
		return nil, fmt.Errorf("创建临时迁移包失败: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if err := buildMigrationZip(tmp, ledger, assetCount, attachments, contents); err != nil {
		_ = tmp.Close()
		cleanup()
		return nil, err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return nil, fmt.Errorf("写入迁移包失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return nil, fmt.Errorf("写入迁移包失败: %w", err)
	}
	if err := os.Rename(tmpName, absPkg); err != nil {
		cleanup()
		return nil, fmt.Errorf("保存迁移包失败: %w", err)
	}
	out := &exportOutcome{
		packagePath: absPkg,
		assetIDs:    ids,
		attachments: len(attachments),
	}
	return out, nil
}

// encodeLedger 以与 store.save 相同的缩进 JSON 形式编码台账数据。
func encodeLedger(d *storeData) ([]byte, error) {
	d.EventsJSON = make([]eventJSON, len(d.Events))
	for i, e := range d.Events {
		d.EventsJSON[i] = e.toJSON()
	}
	if err := validateData(d); err != nil {
		return nil, fmt.Errorf("待打包数据未通过一致性检查: %w", err)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(d); err != nil {
		return nil, fmt.Errorf("编码台账失败: %w", err)
	}
	return buf.Bytes(), nil
}

// decodePackageLedger 解析包内台账 JSON 并转换履历、补齐旧库缺省段，
// 不做任何写盘。
func decodePackageLedger(raw []byte) (*storeData, error) {
	var d storeData
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("包内台账无法解析: %w", err)
	}
	if d.EventsJSON != nil {
		d.Events = make([]Event, len(d.EventsJSON))
		for i, ej := range d.EventsJSON {
			ev, err := ej.toEvent()
			if err != nil {
				return nil, fmt.Errorf("包内台账履历无效: %w", err)
			}
			d.Events[i] = ev
		}
	}
	if d.Plans == nil {
		d.Plans = []*Plan{}
	}
	if d.Parts == nil {
		d.Parts = []*PartWithdrawal{}
	}
	if d.NextPartSeq == 0 && len(d.Parts) == 0 {
		d.NextPartSeq = 1
	}
	if d.Attachments == nil {
		d.Attachments = []*Attachment{}
	}
	if d.NextAttachSeq == 0 && len(d.Attachments) == 0 {
		d.NextAttachSeq = 1
	}
	return &d, nil
}

// readZipMember 读取一个 zip 成员的全部内容。
func readZipMember(f *zip.File) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// verifyPackage 完整核对迁移包：包格式与版本、成员集合（无缺失、额外、重复
// 成员）、成员类型（仅普通文件，拒绝目录与链接）、成员名（拒绝越界路径）、
// 台账格式与业务关联、资料映射、实际成员与内容校验值。任何问题都返回说明
// 原因的错误。核对全程只读包文件，不写出任何内容。
//
// 返回台账数据、附件索引到资料成员名的映射与成员内容（键为成员名）。
func verifyPackage(zr *zip.Reader) (*storeData, map[string]string, map[string][]byte, error) {
	// 成员清单核对：名称唯一、仅普通文件、成员名不越界。
	members := map[string]*zip.File{}
	for _, f := range zr.File {
		name := filepath.ToSlash(f.Name)
		if name == "" || strings.HasPrefix(name, "/") || strings.HasPrefix(name, "../") ||
			name == ".." || strings.Contains(name, "/../") || strings.HasSuffix(name, "/..") ||
			strings.Contains(name, "\\") {
			return nil, nil, nil, fmt.Errorf("迁移包含有越界成员路径 %q，整包拒绝", f.Name)
		}
		if !f.Mode().IsRegular() {
			return nil, nil, nil, fmt.Errorf("迁移包成员 %s 不是普通文件（拒绝目录或链接成员）", name)
		}
		if members[name] != nil {
			return nil, nil, nil, fmt.Errorf("迁移包成员 %s 重复，整包拒绝", name)
		}
		members[name] = f
	}

	manifestFile := members[migrateManifestName]
	if manifestFile == nil {
		return nil, nil, nil, fmt.Errorf("迁移包缺少清单成员 %s", migrateManifestName)
	}
	manifestRaw, err := readZipMember(manifestFile)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("读取清单失败: %w", err)
	}
	var manifest migrateManifest
	// 宽容解析（忽略未知字段）：包的版本判断必须优先于字段差异，未来版本的
	// 包须报“未知版本”原因，而不是笼统的格式错误。包内实际成员集合另按
	// 严格枚举核对，未知字段不会产生任何成员或写出。
	if err := json.Unmarshal(manifestRaw, &manifest); err != nil {
		return nil, nil, nil, fmt.Errorf("清单格式无效: %w", err)
	}
	if manifest.Format != migrateMagic+"/1" {
		return nil, nil, nil, fmt.Errorf("未知迁移包格式 %q（本工具支持 %s/1）", manifest.Format, migrateMagic)
	}
	if manifest.Version != migrateVersion {
		return nil, nil, nil, fmt.Errorf("未知迁移包版本 %d：本工具仅支持版本 %d，整包拒绝", manifest.Version, migrateVersion)
	}

	// 清单内资料映射自洽：附件编号与成员名均唯一，成员名合法。
	wantFiles := map[string]bool{}
	wantAttach := map[string]bool{}
	for _, fm := range manifest.Files {
		if fm.AttachmentID == "" || fm.Member == "" || fm.SHA256 == "" {
			return nil, nil, nil, fmt.Errorf("清单中存在字段不完整的资料条目")
		}
		if wantAttach[fm.AttachmentID] {
			return nil, nil, nil, fmt.Errorf("清单中附件编号 %s 的资料条目重复", fm.AttachmentID)
		}
		wantAttach[fm.AttachmentID] = true
		if wantFiles[fm.Member] {
			return nil, nil, nil, fmt.Errorf("清单中资料成员 %s 重复", fm.Member)
		}
		if !fileMemberPattern.MatchString(fm.Member) {
			return nil, nil, nil, fmt.Errorf("清单中资料成员名 %q 不合法", fm.Member)
		}
		wantFiles[fm.Member] = true
	}

	// 实际成员集合必须恰为 {清单, 台账} ∪ 清单所列资料成员：无缺失、无额外。
	wantMembers := map[string]bool{migrateManifestName: true}
	if manifest.Ledger != migrateLedgerName {
		return nil, nil, nil, fmt.Errorf("清单声明的台账成员名为 %q，与约定 %q 不符", manifest.Ledger, migrateLedgerName)
	}
	wantMembers[migrateLedgerName] = true
	for name := range wantFiles {
		wantMembers[name] = true
	}
	if len(members) != len(wantMembers) {
		return nil, nil, nil, fmt.Errorf("迁移包成员数量不符：应有 %d 个，实际 %d 个（存在缺失或额外成员）",
			len(wantMembers), len(members))
	}
	for name := range wantMembers {
		if members[name] == nil {
			return nil, nil, nil, fmt.Errorf("迁移包缺少成员 %s", name)
		}
	}
	for name := range members {
		if !wantMembers[name] {
			return nil, nil, nil, fmt.Errorf("迁移包含有清单之外的额外成员 %s", name)
		}
	}

	// 台账内容校验值、格式与整库业务关联。
	ledgerRaw, err := readZipMember(members[migrateLedgerName])
	if err != nil {
		return nil, nil, nil, fmt.Errorf("读取台账成员失败: %w", err)
	}
	if int64(len(ledgerRaw)) != manifest.LedgerSize || sha256Hex(ledgerRaw) != manifest.LedgerSHA256 {
		return nil, nil, nil, fmt.Errorf("台账成员内容损坏：大小或校验值与清单不符")
	}
	d, err := decodePackageLedger(ledgerRaw)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := validateData(d); err != nil {
		return nil, nil, nil, fmt.Errorf("包内台账业务关联校验失败: %w", err)
	}
	// 清单汇总数量须与台账实际成员一致。
	if manifest.AssetCount != len(d.Assets) {
		return nil, nil, nil, fmt.Errorf("清单资产数量 %d 与台账实际 %d 不符",
			manifest.AssetCount, len(d.Assets))
	}
	if manifest.AttachmentCount != len(d.Attachments) {
		return nil, nil, nil, fmt.Errorf("清单附件索引数量 %d 与台账实际 %d 不符",
			manifest.AttachmentCount, len(d.Attachments))
	}

	// 资料映射核对：每条附件索引恰有一个资料成员，每个资料成员恰对应一条
	// 附件索引；成员内容的大小与 SHA-256 与清单一致（zip 读取本身已校验 CRC）。
	attachByID := map[string]*Attachment{}
	for _, a := range d.Attachments {
		attachByID[a.ID] = a
	}
	if len(attachByID) != len(d.Attachments) {
		return nil, nil, nil, fmt.Errorf("包内台账附件编号重复")
	}
	if len(manifest.Files) != len(d.Attachments) {
		return nil, nil, nil, fmt.Errorf("资料成员数量（%d）与附件索引数量（%d）不符",
			len(manifest.Files), len(d.Attachments))
	}
	attachMember := map[string]string{}
	contents := map[string][]byte{}
	for _, fm := range manifest.Files {
		if attachByID[fm.AttachmentID] == nil {
			return nil, nil, nil, fmt.Errorf("资料成员 %s 对应的附件编号 %s 在台账中不存在",
				fm.Member, fm.AttachmentID)
		}
		raw, err := readZipMember(members[fm.Member])
		if err != nil {
			return nil, nil, nil, fmt.Errorf("读取资料成员 %s 失败: %w", fm.Member, err)
		}
		if int64(len(raw)) != fm.Size || sha256Hex(raw) != fm.SHA256 {
			return nil, nil, nil, fmt.Errorf("资料成员 %s（附件编号 %s）内容损坏：大小或校验值不符",
				fm.Member, fm.AttachmentID)
		}
		attachMember[fm.AttachmentID] = fm.Member
		contents[fm.Member] = raw
	}
	return d, attachMember, contents, nil
}

// writeAtomicFile 在 dir 下经临时文件 + rename 原子写入一个普通文件，
// 权限 0644；失败由调用方负责清理暂存目录。
func writeAtomicFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".caretrack-restore-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

// restorePackage 从迁移包在尚不存在的 targetDir 还原数据：先在目标的同级
// 暂存目录内建立台账与独立资料副本（附件记录及登记、撤销履历中的路径同步
// 替换为副本的绝对路径），全部写入并复核后才整体改名发布为目标目录。任何
// 失败都不留下目标目录或部分资料；输入包与原数据字节保持，可用原包重试。
func restorePackage(packagePath, targetDir string) (*restoreOutcome, error) {
	absPkg, err := filepath.Abs(packagePath)
	if err != nil {
		return nil, fmt.Errorf("无法解析迁移包路径 %q: %w", packagePath, err)
	}
	absTarget, err := filepath.Abs(targetDir)
	if err != nil {
		return nil, fmt.Errorf("无法解析目标数据目录 %q: %w", targetDir, err)
	}
	// 目标目录（或任何同名路径）已存在即拒绝。
	if _, err := os.Lstat(absTarget); err == nil {
		return nil, fmt.Errorf("目标数据目录 %s 已存在，拒绝覆盖，请选择尚不存在的目录", absTarget)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("无法访问目标数据目录 %s: %w", absTarget, err)
	}

	// 迁移包须为可读普通文件；只读打开，绝不写回。
	pf, err := os.Open(absPkg)
	if err != nil {
		return nil, fmt.Errorf("无法打开迁移包 %s: %w", absPkg, err)
	}
	fi, err := pf.Stat()
	if err != nil {
		_ = pf.Close()
		return nil, fmt.Errorf("无法读取迁移包 %s: %w", absPkg, err)
	}
	if !fi.Mode().IsRegular() {
		_ = pf.Close()
		return nil, fmt.Errorf("迁移包 %s 不是普通文件", absPkg)
	}
	zr, err := zip.NewReader(pf, fi.Size())
	if err != nil {
		_ = pf.Close()
		return nil, fmt.Errorf("迁移包 %s 不是有效的包格式: %w", absPkg, err)
	}
	// 提交前完整核对：格式、版本、业务关联、资料映射、实际成员与校验值。
	d, attachMember, contents, err := verifyPackage(zr)
	if err != nil {
		_ = pf.Close()
		return nil, err
	}
	if err := pf.Close(); err != nil {
		return nil, fmt.Errorf("读取迁移包失败: %w", err)
	}

	// 附件路径替换映射：副本将位于目标目录内的 attachments/ 子目录，文件名
	// 沿用包内生成名；这些路径均由工具生成，绝不取自包内原绝对路径，也不会
	// 按原路径访问外部资料。
	newPath := map[string]string{}
	for _, a := range d.Attachments {
		base := filepath.Base(attachMember[a.ID])
		np := filepath.Join(absTarget, restoreAttachDir, base)
		newPath[a.ID] = np
	}
	for _, a := range d.Attachments {
		a.Path = newPath[a.ID]
	}
	// 登记与撤销履历中的路径同步替换（validateData 已保证履历路径与记录一致，
	// 替换后其余身份与业务含义保持，不新增任何履历）。
	for i := range d.Events {
		e := &d.Events[i]
		if (e.Kind == eventAttach || e.Kind == eventAttachRevoke) && e.AttachmentID != "" {
			if np := newPath[e.AttachmentID]; np != "" {
				e.Path = np
			}
		}
	}
	if err := validateData(d); err != nil {
		return nil, fmt.Errorf("替换资料路径后台账校验失败: %w", err)
	}
	ledger, err := encodeLedger(d)
	if err != nil {
		return nil, err
	}

	// 在目标同级建立暂存目录，所有写入完成并复核后整体 rename 发布；
	// 任何失败都删除暂存目录并确保目标路径不存在。
	parent := filepath.Dir(absTarget)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return nil, fmt.Errorf("创建目标父目录失败: %w", err)
	}
	stage, err := os.MkdirTemp(parent, ".caretrack-restore-*")
	if err != nil {
		return nil, fmt.Errorf("创建暂存目录失败: %w", err)
	}
	abort := func(err error) (*restoreOutcome, error) {
		_ = os.RemoveAll(stage)
		_ = os.RemoveAll(absTarget)
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(stage, restoreAttachDir), 0o755); err != nil {
		return abort(fmt.Errorf("创建资料目录失败: %w", err))
	}
	if err := writeAtomicFile(filepath.Join(stage, dataFileName), ledger); err != nil {
		return abort(fmt.Errorf("写入台账失败: %w", err))
	}
	// 资料副本：逐份写入，成员名由工具生成，全部限制在暂存目录（发布后即
	// 目标目录）之内。
	for _, a := range d.Attachments {
		member := attachMember[a.ID]
		dst := filepath.Join(stage, restoreAttachDir, filepath.Base(member))
		if err := writeAtomicFile(dst, contents[member]); err != nil {
			return abort(fmt.Errorf("写入资料副本失败（附件编号 %s）: %w", a.ID, err))
		}
	}
	// 发布前在暂存目录内复核：台账可加载且业务自洽，每份资料为与包内一致的
	// 普通文件（内容校验值相符）。
	checkStore, err := loadStore(stage, false)
	if err != nil {
		return abort(fmt.Errorf("暂存台账复核失败: %w", err))
	}
	for _, a := range checkStore.data.Attachments {
		member := attachMember[a.ID]
		path := filepath.Join(stage, restoreAttachDir, filepath.Base(member))
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return abort(fmt.Errorf("资料副本复核失败（附件编号 %s）: %w", a.ID, rerr))
		}
		if sha256Hex(raw) != sha256Hex(contents[member]) {
			return abort(fmt.Errorf("资料副本（附件编号 %s）复核校验值不符", a.ID))
		}
	}
	// 整体发布：暂存目录原子改名为目标目录。
	if err := os.Rename(stage, absTarget); err != nil {
		return abort(fmt.Errorf("发布目标目录失败: %w", err))
	}
	// 发布后再复核一次：台账可加载、资料副本为可读普通文件。全部就绪后才
	// 向调用方报告成功；失败则移除目标目录。
	final, err := openSourceStore(absTarget)
	if err != nil {
		_ = os.RemoveAll(absTarget)
		return nil, fmt.Errorf("还原后复核失败: %w", err)
	}
	for _, a := range final.data.Attachments {
		if !attachmentReadable(a.Path) {
			_ = os.RemoveAll(absTarget)
			return nil, fmt.Errorf("还原后复核失败：附件编号 %s 的资料副本不可读", a.ID)
		}
	}
	return &restoreOutcome{
		targetDir:   absTarget,
		assets:      len(final.data.Assets),
		attachments: len(final.data.Attachments),
	}, nil
}

// cmdExport 为“导出迁移包”命令处理：输入数据目录、非空资产编号列表与包
// 文件路径。源台账与原资料只读，已有包文件不得覆盖，任何失败不留成品包。
func cmdExport(args []string, w io.Writer) error {
	var opts cmdOptions
	var packagePath string
	var assetIDs stringListFlag
	fs := newFlagSet("export", &opts)
	fs.StringVar(&packagePath, "package", "", "迁移包文件路径（必填；已存在则拒绝，不会覆盖）")
	fs.Var(&assetIDs, "asset-id", "要打包的资产编号（必填，可重复；重复编号按一项处理）")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := requireFlag(fs, packagePath, "package"); err != nil {
		return err
	}
	if len(assetIDs) == 0 {
		return &usageError{msg: "命令 export 缺少必填参数 --asset-id（至少一项资产编号）"}
	}
	for _, id := range assetIDs {
		if id == "" {
			return &usageError{msg: "命令 export 的 --asset-id 不能为空"}
		}
	}

	outcome, err := exportPackage(opts.dataDir, packagePath, []string(assetIDs))
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "迁移包已生成: %s\n", outcome.packagePath)
	fmt.Fprintf(w, "所选资产: %d 项（%s）\n", len(outcome.assetIDs), strings.Join(outcome.assetIDs, ", "))
	fmt.Fprintf(w, "附件索引: %d 条\n", outcome.attachments)
	return nil
}

// cmdRestore 为“包还原”命令处理：输入包路径和尚不存在的目标数据目录。
// 在目标内建立独立资料副本，路径同步替换为副本绝对路径；失败不留目标。
func cmdRestore(args []string, w io.Writer) error {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	fs.Usage = func() {
		fmt.Fprintf(os.Stdout, "用法: caretrack restore --package 包文件 --target-dir 目录\n\n")
		fs.PrintDefaults()
	}
	var packagePath, targetDir string
	fs.StringVar(&packagePath, "package", "", "迁移包文件路径（必填）")
	fs.StringVar(&targetDir, "target-dir", "", "要创建的目标数据目录（必填；必须尚不存在）")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := requireFlag(fs, packagePath, "package"); err != nil {
		return err
	}
	if err := requireFlag(fs, targetDir, "target-dir"); err != nil {
		return err
	}

	outcome, err := restorePackage(packagePath, targetDir)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "迁移包已还原: %s\n", outcome.targetDir)
	fmt.Fprintf(w, "资产: %d 项\n", outcome.assets)
	fmt.Fprintf(w, "附件索引: %d 条\n", outcome.attachments)
	return nil
}
