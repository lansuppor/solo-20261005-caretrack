package main

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// 离线资产迁移包：把所选资产及其台账数据与附件资料内容打包为单个本地文件
// （export），并可在另一数据目录还原为独立台账（restore）。全程不依赖外部
// 服务，源台账与原资料只读。
//
// 包格式（版本 1，tar 归档）：
//   - manifest.json    包清单：格式版本、台账成员与每条资料成员的名称、字节数
//     及 SHA-256 校验值
//   - caretrack.json   所选资产的台账子集，与数据文件同格式：保留业务内容、
//     状态、各类编号、履历序号与原计数器；位置起点、报修地点、保养方案段、
//     撤销引用、履历顺序与时间精度保持
//   - materials/Axxxx  每条附件索引（含已撤销）对应的资料内容；一条索引一个
//     成员，同路径的多条索引各自独立
//
// 还原时提交前完整核对包格式、版本、台账业务关联、资料映射、实际成员与校验
// 值；缺失、额外或重复成员、内容损坏、越界成员路径及链接成员均拒绝。成功在
// 目标目录内建立独立资料副本，附件记录及对应登记、撤销履历中的路径同步替换
// 为副本的绝对路径，其余身份与业务含义保持，不新增业务履历。
const (
	packVersion      = 1
	packManifestName = "manifest.json"
	packLedgerName   = dataFileName // caretrack.json
	packMaterialDir  = "materials"
)

// packFile 为清单中台账成员的描述：成员名、字节数与 SHA-256 校验值。
type packFile struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// packMaterial 为清单中一条资料成员的描述：所属附件编号、成员名、字节数与
// SHA-256 校验值。
type packMaterial struct {
	AttachmentID string `json:"attachment_id"`
	Name         string `json:"name"`
	Size         int64  `json:"size"`
	SHA256       string `json:"sha256"`
}

// packManifest 为迁移包清单：格式版本、台账成员与全部资料成员的校验信息。
type packManifest struct {
	Version   int            `json:"version"`
	Ledger    packFile       `json:"ledger"`
	Materials []packMaterial `json:"materials"`
}

// packMaterialData 为待写入包内的一条资料内容。
type packMaterialData struct {
	id      string
	content []byte
}

// packOutcome 为一次成功导出或还原的结果摘要，用于输出。
type packOutcome struct {
	assetIDs    []string
	attachments int
}

// sha256Hex 返回内容的 SHA-256 校验值（十六进制）。
func sha256Hex(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// cmdExport 打包导出：输入数据目录、非空资产编号列表与包文件路径，把所选资产
// 及其全部台账数据与附件资料内容写为单个迁移包。源台账与原资料只读；已有包
// 文件不得覆盖。成功显示所选资产及附件索引数量。
func cmdExport(args []string, w io.Writer) error {
	var opts cmdOptions
	var pkgPath string
	var assetIDs stringListFlag
	fs := newFlagSet("export", &opts)
	fs.StringVar(&pkgPath, "package", "", "迁移包文件路径（必填；不得指向已存在的文件，不覆盖）")
	fs.Var(&assetIDs, "asset-id", "要打包的资产编号（必填，可重复；重复编号按一项处理）")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := requireFlag(fs, pkgPath, "package"); err != nil {
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

	outcome, err := exportAssets(opts.dataDir, []string(assetIDs), pkgPath)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "已打包 %d 项资产（%s）。\n", len(outcome.assetIDs), strings.Join(outcome.assetIDs, ", "))
	fmt.Fprintf(w, "附件索引: %d 条（对应资料内容已随包保存）。\n", outcome.attachments)
	fmt.Fprintf(w, "迁移包: %s\n", pkgPath)
	return nil
}

// cmdRestore 包还原：输入包路径与尚不存在的目标数据目录，完整核对后在目标内
// 建立独立资料副本并写出台账。失败不留下目标目录或部分资料，恢复条件后可用
// 原包重试。成功显示资产及附件索引数量。
func cmdRestore(args []string, w io.Writer) error {
	var opts cmdOptions
	var pkgPath string
	fs := newFlagSet("restore", &opts)
	fs.StringVar(&pkgPath, "package", "", "迁移包文件路径（必填，只读）")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := requireFlag(fs, pkgPath, "package"); err != nil {
		return err
	}

	outcome, err := restorePackage(pkgPath, opts.dataDir)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "已还原 %d 项资产（%s）。\n", len(outcome.assetIDs), strings.Join(outcome.assetIDs, ", "))
	fmt.Fprintf(w, "附件索引: %d 条（资料副本已建立在目标目录内）。\n", outcome.attachments)
	fmt.Fprintf(w, "数据目录: %s\n", opts.dataDir)
	return nil
}

// exportAssets 把 dataDir 中编号为 assetIDs 的资产打包到 pkgPath。
// 源台账与原资料只读；任何失败都不留下成品包。
func exportAssets(dataDir string, assetIDs []string, pkgPath string) (*packOutcome, error) {
	// 已有包文件不得覆盖：先快速失败，发布时再以原子链接兜底。
	if _, err := os.Stat(pkgPath); err == nil {
		return nil, fmt.Errorf("迁移包文件 %s 已存在：不覆盖已有包文件", pkgPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("无法确认迁移包路径 %s: %w", pkgPath, err)
	}
	// 源台账必须已存在并通过整库一致性检查；源台账始终只读，绝不写入。
	src, err := openSourceStore(dataDir)
	if err != nil {
		return nil, err
	}
	// 重复编号按一项处理，保持首次出现的顺序；未知资产整次拒绝。
	selected := map[string]bool{}
	ids := make([]string, 0, len(assetIDs))
	for _, id := range assetIDs {
		if !selected[id] {
			selected[id] = true
			ids = append(ids, id)
		}
	}
	for _, id := range ids {
		if src.findAsset(id) == nil {
			return nil, fmt.Errorf("%w: 源台账中不存在资产编号 %q", errNotFound, id)
		}
	}
	subset := subsetStoreData(src.data, selected)
	// 每条附件索引（含已撤销）指向的资料当前都须为可读普通文件，否则整次
	// 拒绝并说明附件编号与原因，不把缺失资料当成完整迁移。原资料只读。
	materials := make([]packMaterialData, 0, len(subset.Attachments))
	for _, a := range subset.Attachments {
		if err := checkAttachFile(a.Path); err != nil {
			return nil, fmt.Errorf("附件 %s 的资料无法打包：%w", a.ID, err)
		}
		content, err := os.ReadFile(a.Path)
		if err != nil {
			return nil, fmt.Errorf("附件 %s 的资料 %s 读取失败: %w", a.ID, a.Path, err)
		}
		materials = append(materials, packMaterialData{id: a.ID, content: content})
	}
	// 台账子集与数据文件同格式：保留原编号、履历序号与计数器；履历时间
	// 保留小数秒精度。子集取自已通过整库校验的台账，此处再核对一次。
	if err := validateData(subset); err != nil {
		return nil, fmt.Errorf("待打包数据未通过一致性检查：%w", err)
	}
	ledger, err := encodeStoreData(subset)
	if err != nil {
		return nil, err
	}
	if err := writePackageFile(pkgPath, ledger, materials); err != nil {
		return nil, err
	}
	return &packOutcome{assetIDs: ids, attachments: len(subset.Attachments)}, nil
}

// subsetStoreData 抽取所选资产的台账子集：资产及其全部工单、请求绑定、履历、
// 保养计划、备件记录与附件索引，保留原数组顺序、各类编号、履历序号与原计数器。
func subsetStoreData(src *storeData, selected map[string]bool) *storeData {
	d := &storeData{
		Version:       src.Version,
		Assets:        []*Asset{},
		Tickets:       []*Ticket{},
		Events:        []Event{},
		EventsJSON:    []eventJSON{},
		Requests:      []requestBinding{},
		Plans:         []*Plan{},
		Parts:         []*PartWithdrawal{},
		Attachments:   []*Attachment{},
		NextTicketSeq: src.NextTicketSeq,
		NextPartSeq:   src.NextPartSeq,
		NextAttachSeq: src.NextAttachSeq,
	}
	for _, a := range src.Assets {
		if selected[a.ID] {
			na := *a
			d.Assets = append(d.Assets, &na)
		}
	}
	for _, t := range src.Tickets {
		if selected[t.AssetID] {
			nt := *t
			d.Tickets = append(d.Tickets, &nt)
		}
	}
	for _, e := range src.Events {
		if selected[e.AssetID] {
			d.Events = append(d.Events, e)
		}
	}
	for _, r := range src.Requests {
		if selected[r.AssetID] {
			d.Requests = append(d.Requests, r)
		}
	}
	for _, p := range src.Plans {
		if selected[p.AssetID] {
			np := *p
			d.Plans = append(d.Plans, &np)
		}
	}
	for _, p := range src.Parts {
		if selected[p.AssetID] {
			np := *p
			d.Parts = append(d.Parts, &np)
		}
	}
	for _, a := range src.Attachments {
		if selected[a.AssetID] {
			na := *a
			d.Attachments = append(d.Attachments, &na)
		}
	}
	return d
}

// writePackageFile 把清单、台账与资料成员写为单个迁移包文件：先写同目录临时
// 文件，fsync 后以原子链接发布——目标已存在时链接失败，绝不覆盖已有包文件；
// 任何失败都移除临时文件，不留下成品包。
func writePackageFile(pkgPath string, ledger []byte, materials []packMaterialData) error {
	manifest := packManifest{
		Version:   packVersion,
		Ledger:    packFile{Name: packLedgerName, Size: int64(len(ledger)), SHA256: sha256Hex(ledger)},
		Materials: make([]packMaterial, 0, len(materials)),
	}
	for _, m := range materials {
		manifest.Materials = append(manifest.Materials, packMaterial{
			AttachmentID: m.id,
			Name:         packMaterialDir + "/" + m.id,
			Size:         int64(len(m.content)),
			SHA256:       sha256Hex(m.content),
		})
	}
	manifestRaw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("编码迁移包清单失败: %w", err)
	}

	dir := filepath.Dir(pkgPath)
	tmp, err := os.CreateTemp(dir, ".caretrack-pack-*.tmp")
	if err != nil {
		return fmt.Errorf("创建迁移包临时文件失败: %w", err)
	}
	tmpName := tmp.Name()
	abort := func(err error) error {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	tw := tar.NewWriter(tmp)
	writeMember := func(name string, content []byte) error {
		hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		_, err := tw.Write(content)
		return err
	}
	if err := writeMember(packManifestName, manifestRaw); err != nil {
		abort(nil)
		return fmt.Errorf("写入迁移包失败: %w", err)
	}
	if err := writeMember(packLedgerName, ledger); err != nil {
		abort(nil)
		return fmt.Errorf("写入迁移包失败: %w", err)
	}
	for _, m := range materials {
		if err := writeMember(packMaterialDir+"/"+m.id, m.content); err != nil {
			abort(nil)
			return fmt.Errorf("写入迁移包失败: %w", err)
		}
	}
	if err := tw.Close(); err != nil {
		abort(nil)
		return fmt.Errorf("写入迁移包失败: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		abort(nil)
		return fmt.Errorf("写入迁移包失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("写入迁移包失败: %w", err)
	}
	// 原子发布且不覆盖：硬链接在目标已存在时失败。
	if err := os.Link(tmpName, pkgPath); err != nil {
		_ = os.Remove(tmpName)
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("迁移包文件 %s 已存在：不覆盖已有包文件", pkgPath)
		}
		return fmt.Errorf("发布迁移包失败: %w", err)
	}
	_ = os.Remove(tmpName)
	return nil
}

// restorePackage 把 pkgPath 指向的迁移包还原到尚不存在的 targetDir。
// 输入包只读；任何失败都不留下目标目录或部分资料，恢复条件后可用原包重试。
func restorePackage(pkgPath, targetDir string) (*packOutcome, error) {
	// 目标数据目录必须尚不存在：已有目录（或同名文件）即拒绝。
	if _, err := os.Stat(targetDir); err == nil {
		return nil, fmt.Errorf("目标数据目录 %s 已存在：还原只写入全新目录", targetDir)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("无法确认目标数据目录 %s: %w", targetDir, err)
	}
	raw, err := os.ReadFile(pkgPath)
	if err != nil {
		return nil, fmt.Errorf("读取迁移包失败: %w", err)
	}
	// 提交前完整核对：包格式、版本、台账业务关联、资料映射、实际成员与校验值。
	ledger, materials, err := verifyPackage(raw)
	if err != nil {
		return nil, err
	}

	targetAbs, err := filepath.Abs(targetDir)
	if err != nil {
		return nil, fmt.Errorf("无法解析目标数据目录: %w", err)
	}
	// 路径替换：附件记录及对应登记、撤销履历中的路径同步替换为目标内副本的
	// 绝对路径；其余身份与业务含义保持，不新增业务履历。还原只使用包内成员，
	// 不按包内原绝对路径访问外部资料。
	newPaths := make(map[string]string, len(ledger.Attachments))
	for _, a := range ledger.Attachments {
		newPaths[a.ID] = filepath.Join(targetAbs, packMaterialDir, a.ID)
	}
	for _, a := range ledger.Attachments {
		a.Path = newPaths[a.ID]
	}
	for i := range ledger.Events {
		e := &ledger.Events[i]
		if e.Kind == eventAttach || e.Kind == eventAttachRevoke {
			e.Path = newPaths[e.AttachmentID]
		}
	}

	// 落盘：先在目标内建立全部资料副本，再原子写出台账；所有资料与台账完整
	// 就绪后才公布成功。任何失败都移除整个目标目录，不留部分结果。
	if err := os.MkdirAll(filepath.Join(targetAbs, packMaterialDir), 0o755); err != nil {
		return nil, fmt.Errorf("创建目标数据目录失败: %w", err)
	}
	cleanup := func(err error) (*packOutcome, error) {
		_ = os.RemoveAll(targetAbs)
		return nil, err
	}
	for _, a := range ledger.Attachments {
		dst := newPaths[a.ID]
		// 副本路径必须位于目标目录内（附件编号已限定为 A 加补零序号的形式，
		// 此处再防御一次），不能写出目标目录。
		if !withinDir(targetAbs, dst) {
			return cleanup(fmt.Errorf("附件 %s 的副本路径越出目标数据目录", a.ID))
		}
		if err := writeFileSync(dst, materials[a.ID], 0o644); err != nil {
			return cleanup(fmt.Errorf("写入附件 %s 的资料副本失败: %w", a.ID, err))
		}
	}
	s := &store{dir: targetAbs, data: ledger, now: time.Now}
	if err := s.save(); err != nil {
		return cleanup(err)
	}
	ids := make([]string, 0, len(ledger.Assets))
	for _, a := range ledger.Assets {
		ids = append(ids, a.ID)
	}
	return &packOutcome{assetIDs: ids, attachments: len(ledger.Attachments)}, nil
}

// verifyPackage 完整核对迁移包：格式、版本、实际成员集合、各成员校验值、台账
// 业务关联与资料映射。全部通过才返回台账数据与资料内容（附件编号 -> 内容）。
func verifyPackage(raw []byte) (*storeData, map[string][]byte, error) {
	members := map[string][]byte{}
	tr := tar.NewReader(bytes.NewReader(raw))
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("迁移包已损坏：读取成员失败: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA {
			return nil, nil, fmt.Errorf("迁移包已损坏：成员 %q 不是普通文件（链接或其他类型成员不接受）", hdr.Name)
		}
		name := hdr.Name
		// 越界成员路径拒绝：成员名须为干净的相对路径。
		if name == "" || path.IsAbs(name) || path.Clean(name) != name ||
			name == ".." || strings.HasPrefix(name, "../") {
			return nil, nil, fmt.Errorf("迁移包已损坏：成员路径 %q 越界或无效", name)
		}
		if members[name] != nil {
			return nil, nil, fmt.Errorf("迁移包已损坏：成员 %q 重复", name)
		}
		content, err := io.ReadAll(tr)
		if err != nil {
			return nil, nil, fmt.Errorf("迁移包已损坏：读取成员 %q 失败: %w", name, err)
		}
		if int64(len(content)) != hdr.Size {
			return nil, nil, fmt.Errorf("迁移包已损坏：成员 %q 的字节数与头部记录不符", name)
		}
		members[name] = content
	}

	manifestRaw, ok := members[packManifestName]
	if !ok {
		return nil, nil, fmt.Errorf("迁移包已损坏：缺少清单成员 %s", packManifestName)
	}
	var manifest packManifest
	if err := json.Unmarshal(manifestRaw, &manifest); err != nil {
		return nil, nil, fmt.Errorf("迁移包已损坏：清单无法解析: %w", err)
	}
	if manifest.Version != packVersion {
		return nil, nil, fmt.Errorf("未知的迁移包版本 %d（本工具支持版本 %d）", manifest.Version, packVersion)
	}
	// 台账成员：名称固定，内容与清单校验值一致。
	if manifest.Ledger.Name != packLedgerName {
		return nil, nil, fmt.Errorf("迁移包已损坏：清单中的台账成员名 %q 无效", manifest.Ledger.Name)
	}
	expected := map[string]bool{packManifestName: true, packLedgerName: true}
	ledgerRaw, ok := members[packLedgerName]
	if !ok {
		return nil, nil, fmt.Errorf("迁移包已损坏：缺少台账成员 %s", packLedgerName)
	}
	if err := checkPackDigest(manifest.Ledger.SHA256, manifest.Ledger.Size, ledgerRaw); err != nil {
		return nil, nil, fmt.Errorf("迁移包已损坏：台账成员校验失败：%w", err)
	}
	// 资料成员：每条附件索引恰有一个成员，成员名固定且不得越界。
	materials := map[string][]byte{}
	for _, m := range manifest.Materials {
		if _, ok := parseAttachSeq(m.AttachmentID); !ok {
			return nil, nil, fmt.Errorf("迁移包已损坏：清单中的附件编号 %q 无效", m.AttachmentID)
		}
		if materials[m.AttachmentID] != nil {
			return nil, nil, fmt.Errorf("迁移包已损坏：清单中附件 %s 的资料映射重复", m.AttachmentID)
		}
		wantName := packMaterialDir + "/" + m.AttachmentID
		if m.Name != wantName {
			return nil, nil, fmt.Errorf("迁移包已损坏：清单中附件 %s 的成员名 %q 无效（应为 %s）",
				m.AttachmentID, m.Name, wantName)
		}
		content, ok := members[m.Name]
		if !ok {
			return nil, nil, fmt.Errorf("迁移包已损坏：缺少附件 %s 的资料成员 %s", m.AttachmentID, m.Name)
		}
		if err := checkPackDigest(m.SHA256, m.Size, content); err != nil {
			return nil, nil, fmt.Errorf("迁移包已损坏：附件 %s 的资料校验失败：%w", m.AttachmentID, err)
		}
		expected[m.Name] = true
		materials[m.AttachmentID] = content
	}
	// 实际成员不得多于或少于清单期望：额外成员（含越界命名）一律拒绝。
	for name := range members {
		if !expected[name] {
			return nil, nil, fmt.Errorf("迁移包已损坏：存在额外成员 %q", name)
		}
	}
	// 台账业务关联：与数据文件同一套整库一致性校验。
	ledger, err := decodeStoreData(ledgerRaw)
	if err != nil {
		return nil, nil, fmt.Errorf("迁移包中的台账%w", err)
	}
	// 资料映射与台账附件索引一一对应（含已撤销索引）。
	if len(ledger.Attachments) != len(materials) {
		return nil, nil, fmt.Errorf(
			"迁移包已损坏：台账附件索引 %d 条与清单资料映射 %d 条不符",
			len(ledger.Attachments), len(materials))
	}
	for _, a := range ledger.Attachments {
		if materials[a.ID] == nil {
			return nil, nil, fmt.Errorf("迁移包已损坏：附件 %s 缺少对应的资料成员", a.ID)
		}
	}
	return ledger, materials, nil
}

// checkPackDigest 核对成员内容的字节数与 SHA-256 校验值。
func checkPackDigest(wantSHA string, wantSize int64, content []byte) error {
	if int64(len(content)) != wantSize {
		return fmt.Errorf("字节数 %d 与清单记录的 %d 不符", len(content), wantSize)
	}
	if sha256Hex(content) != wantSHA {
		return errors.New("SHA-256 校验值与清单记录不符")
	}
	return nil
}

// withinDir 报告 p 是否位于 dir 之内。
func withinDir(dir, p string) bool {
	rel, err := filepath.Rel(dir, p)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// writeFileSync 以排他创建写入文件并 fsync：目标已存在时失败，不覆盖。
func writeFileSync(name string, content []byte, perm os.FileMode) error {
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(content); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
