// SCode Desktop 一键打包脚本（Windows x64）
//
// 产物（输出到 release/）：
//   1. 便携目录  release/SCode-win-x64/                  —— 解压即用
//   2. 解压包    release/SCode-<ver>-win-x64-portable.zip
//   3. 安装包    release/SCode-Setup-<ver>.exe           —— 需要 NSIS（makensis）
//
// 用法：
//   node scripts/package.mjs                 # 全部产物
//   node scripts/package.mjs --skip-build    # 跳过 vite 构建（dist-renderer 已是最新时）
//   node scripts/package.mjs --only=zip      # 只出 zip（dir|zip|installer 逗号组合）
//   node scripts/package.mjs --scode-bin=路径 # 指定后端 scode.exe（默认 ../dist/scode.exe）
//
// 中文兼容说明：
//   - 纯 Node 实现，不经过 .bat/cmd 的 GBK 代码页，中文路径/中文输出不乱码；
//   - zip 用 PowerShell Compress-Archive（.NET 会置 UTF-8 文件名标志位），
//     中文 Windows / 7-Zip / Bandizip 解压均不乱码；
//   - NSIS 脚本以 UTF-8 BOM 写入并开启 `Unicode true`，安装界面中文正常。

import { execFileSync, execSync } from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const root = path.resolve(__dirname, '..');           // desktop/
const pkg = JSON.parse(fs.readFileSync(path.join(root, 'package.json'), 'utf8'));
const VER = pkg.version;
const APP = pkg.productName || 'SCode';

// ---- 参数 -----------------------------------------------------------------
const argv = process.argv.slice(2);
const has = name => argv.includes(`--${name}`);
const argVal = name => {
  const a = argv.find(a => a.startsWith(`--${name}=`));
  return a ? a.slice(name.length + 3) : null;
};
const only = argVal('only') ? argVal('only').split(',') : ['dir', 'zip', 'installer'];

const log = msg => console.log(`[package] ${msg}`);
const die = msg => { console.error(`[package] 错误: ${msg}`); process.exit(1); };

if (process.platform !== 'win32') die('当前脚本面向 Windows 打包（Electron dist 为 win-x64）');

// ---- 路径 -----------------------------------------------------------------
const releaseDir = path.join(root, 'release');
const stageDir = path.join(releaseDir, `${APP}-win-x64`);
const zipFile = path.join(releaseDir, `${APP}-${VER}-win-x64-portable.zip`);
const setupFile = path.join(releaseDir, `${APP}-Setup-${VER}.exe`);
const electronDist = path.join(root, 'node_modules', 'electron', 'dist');

// 后端二进制：优先参数，其次环境变量，默认 ../dist/scode.exe
const scodeBin = argVal('scode-bin')
  || process.env.SCODE_BIN
  || path.join(root, '..', 'dist', 'scode.exe');

// 重试删除：杀软/索引器/残留进程可能短暂占用文件导致 EPERM/EBUSY
const rmrf = p => fs.rmSync(p, { recursive: true, force: true, maxRetries: 10, retryDelay: 500 });
const cp = (src, dst) => fs.cpSync(src, dst, { recursive: true });

// ---- 1. 构建渲染层 ----------------------------------------------------------
if (!has('skip-build')) {
  log('构建渲染层 (vite build)…');
  // Windows 上 npm 是 .cmd，必须经 shell 调用；shell:true 不涉及中文编码问题
  execSync('npm run build', { cwd: root, stdio: 'inherit', shell: true });
} else {
  log('跳过渲染层构建 (--skip-build)');
}
if (!fs.existsSync(path.join(root, 'dist-renderer', 'index.html'))) {
  die('dist-renderer/index.html 不存在，请先 npm run build');
}
if (!fs.existsSync(path.join(electronDist, 'electron.exe'))) {
  die('未找到 node_modules/electron/dist/electron.exe，请先 npm install');
}
const hasBackend = fs.existsSync(scodeBin);
if (!hasBackend) {
  console.warn(`[package] 警告: 未找到后端二进制 ${scodeBin}`);
  console.warn('[package]        打包后将回退到系统 PATH 中的 scode；可用 --scode-bin= 指定');
}

// ---- 2. 组装便携目录 --------------------------------------------------------
log(`组装便携目录 → ${path.relative(root, stageDir)}`);
rmrf(stageDir);
fs.mkdirSync(path.join(stageDir, 'resources', 'app'), { recursive: true });

// 2.1 Electron 运行时（去掉默认 demo 应用，位于 resources/default_app.asar）
for (const entry of fs.readdirSync(electronDist)) {
  if (entry === 'default_app.asar') continue;
  cp(path.join(electronDist, entry), path.join(stageDir, entry));
}
rmrf(path.join(stageDir, 'resources', 'default_app.asar'));
// 清理不需要的 locale 可在此裁剪；默认全部保留以兼容中文系统
fs.renameSync(path.join(stageDir, 'electron.exe'), path.join(stageDir, `${APP}.exe`));

// 2.2 应用本身：package.json(精简) + src + dist-renderer
const appDir = path.join(stageDir, 'resources', 'app');
fs.writeFileSync(
  path.join(appDir, 'package.json'),
  JSON.stringify(
    { name: pkg.name, productName: APP, version: VER, main: 'src/main.js', private: true },
    null,
    2,
  ) + '\n',
  'utf8',
);
cp(path.join(root, 'src'), path.join(appDir, 'src'));
cp(path.join(root, 'dist-renderer'), path.join(appDir, 'dist-renderer'));

// 2.3 后端二进制 → resources/dist/scode.exe（与 src/main.js 的 binPath() 对应）
if (hasBackend) {
  const dst = path.join(stageDir, 'resources', 'dist');
  fs.mkdirSync(dst, { recursive: true });
  fs.copyFileSync(scodeBin, path.join(dst, 'scode.exe'));
  log(`已内置后端 ${scodeBin}`);
}

// 说明文件（中文 UTF-8）
fs.writeFileSync(
  path.join(stageDir, '使用说明.txt'),
  [
    `${APP} 便携版 v${VER}`,
    '',
    `双击 ${APP}.exe 即可运行，无需安装。`,
    '本版本已内置 scode 后端（resources/dist/scode.exe）。',
    '如需使用系统 PATH 中的 scode，删除 resources/dist/scode.exe 即可。',
    '',
  ].join('\r\n'),
  'utf8',
);

// ---- 3. zip 解压包 -----------------------------------------------------------
if (only.includes('zip')) {
  log(`生成解压包 → ${path.relative(root, zipFile)}`);
  rmrf(zipFile);
  // PowerShell Compress-Archive 基于 .NET，zip 条目带 UTF-8 标志位，中文不乱码。
  // -LiteralPath 保证中文/空格路径安全；命令用单引号包裹并转义。
  const ps = (s) => `'${s.replace(/'/g, "''")}'`;
  execFileSync('powershell', [
    '-NoProfile', '-NonInteractive', '-Command',
    `Compress-Archive -LiteralPath ${ps(stageDir)} -DestinationPath ${ps(zipFile)} -CompressionLevel Optimal`,
  ], { stdio: 'inherit' });
}

// ---- 4. NSIS 安装包 ----------------------------------------------------------
if (only.includes('installer')) {
  const makensis = which('makensis') || which('makensis.exe')
    || (fs.existsSync('C:/Program Files (x86)/NSIS/makensis.exe') ? 'C:/Program Files (x86)/NSIS/makensis.exe' : null);

  const nsiFile = path.join(releaseDir, 'installer.nsi');
  // UTF-8 BOM + Unicode true：中文安装界面不乱码
  fs.writeFileSync(nsiFile, '﻿' + nsisScript(), 'utf8');

  if (!makensis) {
    console.warn('[package] 警告: 未找到 makensis（NSIS），跳过安装包编译。');
    console.warn('[package]        安装 NSIS 后执行: makensis release\\installer.nsi');
    console.warn(`[package]        NSIS 脚本已生成: ${path.relative(root, nsiFile)}`);
  } else {
    log(`编译安装包 → ${path.relative(root, setupFile)}`);
    execFileSync(makensis, [nsiFile], { stdio: 'inherit' });
  }
}

log('完成 ✔ 产物见 release/');

// ---- 辅助 --------------------------------------------------------------------
function which(cmd) {
  try {
    const out = execSync(`where ${cmd}`, { encoding: 'utf8', stdio: ['ignore', 'pipe', 'ignore'] });
    return out.split(/\r?\n/)[0].trim() || null;
  } catch { return null; }
}

function nsisScript() {
  // 注意：NSIS 内嵌路径一律用绝对路径，避免相对脚本目录解析在中文路径下出错
  const stage = stageDir.replace(/\//g, '\\');
  const out = setupFile.replace(/\//g, '\\');
  return `
; SCode 安装程序 — 由 scripts/package.mjs 自动生成
Unicode true
!define APPNAME "${APP}"
!define VERSION "${VER}"
!define PUBLISHER "SCode"

Name "\${APPNAME} \${VERSION}"
OutFile "${out}"
; 免管理员权限的每用户安装（中文 Windows 下 $LOCALAPPDATA 可能含中文用户名）
InstallDir "$LOCALAPPDATA\\Programs\\\${APPNAME}"
RequestExecutionLevel user
SetCompressor /SOLID lzma

!include "MUI2.nsh"
!insertmacro MUI_PAGE_WELCOME
!insertmacro MUI_PAGE_DIRECTORY
!insertmacro MUI_PAGE_INSTFILES
!insertmacro MUI_PAGE_FINISH
!insertmacro MUI_UNPAGE_CONFIRM
!insertmacro MUI_UNPAGE_INSTFILES
; 跟随系统语言，中文系统显示中文界面
!insertmacro MUI_LANGUAGE "SimpChinese"
!insertmacro MUI_LANGUAGE "English"

Section "Install"
  SetOutPath "$INSTDIR"
  File /r "${stage}\\*.*"
  ; 如需覆盖系统 PATH 里的 scode，可在此写注册表/环境变量
  CreateDirectory "$SMPROGRAMS\\\${APPNAME}"
  CreateShortcut  "$SMPROGRAMS\\\${APPNAME}\\\${APPNAME}.lnk" "$INSTDIR\\${APP}.exe"
  CreateShortcut  "$DESKTOP\\\${APPNAME}.lnk" "$INSTDIR\\${APP}.exe"
  WriteUninstaller "$INSTDIR\\卸载 \${APPNAME}.exe"
  ; “应用和功能”里的卸载入口
  WriteRegStr HKCU "Software\\Microsoft\\Windows\\CurrentVersion\\Uninstall\\\${APPNAME}" \\
      "DisplayName" "\${APPNAME}"
  WriteRegStr HKCU "Software\\Microsoft\\Windows\\CurrentVersion\\Uninstall\\\${APPNAME}" \\
      "DisplayVersion" "\${VERSION}"
  WriteRegStr HKCU "Software\\Microsoft\\Windows\\CurrentVersion\\Uninstall\\\${APPNAME}" \\
      "Publisher" "\${PUBLISHER}"
  WriteRegStr HKCU "Software\\Microsoft\\Windows\\CurrentVersion\\Uninstall\\\${APPNAME}" \\
      "UninstallString" "$INSTDIR\\卸载 \${APPNAME}.exe"
SectionEnd

Section "Uninstall"
  RMDir /r "$INSTDIR"
  Delete  "$SMPROGRAMS\\\${APPNAME}\\\${APPNAME}.lnk"
  RMDir   "$SMPROGRAMS\\\${APPNAME}"
  Delete  "$DESKTOP\\\${APPNAME}.lnk"
  DeleteRegKey HKCU "Software\\Microsoft\\Windows\\CurrentVersion\\Uninstall\\\${APPNAME}"
SectionEnd
`;
}
