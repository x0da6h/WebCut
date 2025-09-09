package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/jchv/go-webview2"
)

var (
	currentScreenshot []byte
	batchScreenshots  = make(map[string][]byte)
	serverAddr        string
	batchMutex        sync.Mutex
)

// 检查WebView2 Runtime是否已安装
func checkWebView2Runtime() bool {
	// 在Windows上检查WebView2 Runtime
	if runtime.GOOS != "windows" {
		return false
	}

	// 检查常见的WebView2 Runtime安装路径
	programFiles := os.Getenv("ProgramFiles")
	programFilesX86 := os.Getenv("ProgramFiles(x86)")

	commonPaths := []string{
		filepath.Join(programFiles, "Microsoft", "EdgeWebView", "Application", "*", "msedgewebview2.exe"),
		filepath.Join(programFilesX86, "Microsoft", "EdgeWebView", "Application", "*", "msedgewebview2.exe"),
	}

	for _, pathPattern := range commonPaths {
		matches, err := filepath.Glob(pathPattern)
		if err == nil && len(matches) > 0 {
			return true
		}
	}

	// 尝试通过命令行检查
	cmd := exec.Command("reg", "query", "HKLM\\SOFTWARE\\Microsoft\\EdgeUpdate\\Clients\\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}", "/v", "pv")
	output, err := cmd.CombinedOutput()
	if err == nil && strings.Contains(string(output), "REG_SZ") {
		return true
	}

	return false
}

// 下载WebView2 Runtime安装包
func downloadWebView2Runtime(installerPath string) error {
	fmt.Println("正在下载WebView2 Runtime安装包...")

	// WebView2 Runtime下载URL - 使用更可靠的分发链接
	url := "https://msedge.sf.dl.delivery.mp.microsoft.com/filestreamingservice/files/14331884-43a1-464c-bf5e-c9a51e141e37/MicrosoftEdgeWebview2Setup.exe"

	// 检测系统架构
	fmt.Printf("检测到系统架构: %s\n", runtime.GOARCH)

	// 发送HTTP请求
	fmt.Printf("正在从 %s 下载安装包...\n", url)
	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("下载失败: %v\n请尝试手动下载: https://developer.microsoft.com/zh-cn/microsoft-edge/webview2/", err)
	}
	defer resp.Body.Close()

	// 创建安装包文件
	out, err := os.Create(installerPath)
	if err != nil {
		return fmt.Errorf("无法创建安装包文件: %v", err)
	}
	defer out.Close()

	// 获取文件大小
	totalSize, _ := strconv.Atoi(resp.Header.Get("Content-Length"))
	downloaded := 0
	done := false

	// 使用通道报告下载进度
	go func() {
		for !done {
			var progress int
			if totalSize > 0 {
				progress = int(float64(downloaded) / float64(totalSize) * 100)
			} else {
				progress = downloaded / 1024 / 1024 // 显示已下载的MB数
			}
			fmt.Printf("\r下载进度: %d%%", progress)
			time.Sleep(1 * time.Second)
		}
	}()

	// 复制数据并计算进度
	data := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(data)
		if n > 0 {
			if _, err := out.Write(data[:n]); err != nil {
				done = true
				return fmt.Errorf("写入文件失败: %v", err)
			}
			downloaded += n
		}
		if err != nil {
			if err != io.EOF {
				done = true
				return fmt.Errorf("读取数据失败: %v", err)
			}
			break
		}
	}
	done = true
	fmt.Println("\r下载完成!")

	return nil
}

// 检查Windows Installer服务状态
func checkWindowsInstallerService() bool {
	fmt.Println("正在检查Windows Installer服务状态...")
	cmd := exec.Command("sc", "query", "msiserver")
	output, err := cmd.CombinedOutput()
	if err != nil {
		fmt.Printf("警告: 无法检查Windows Installer服务: %v\n", err)
		return false
	}

	status := string(output)
	if strings.Contains(status, "RUNNING") {
		fmt.Println("Windows Installer服务正在运行")
		return true
	} else if strings.Contains(status, "STOPPED") {
		fmt.Println("Windows Installer服务已停止，尝试启动...")
		startCmd := exec.Command("sc", "start", "msiserver")
		startOutput, startErr := startCmd.CombinedOutput()
		if startErr != nil {
			fmt.Printf("警告: 无法启动Windows Installer服务: %v\n输出: %s\n", startErr, string(startOutput))
			return false
		}
		fmt.Println("Windows Installer服务已成功启动")
		return true
	}

	fmt.Printf("警告: Windows Installer服务状态未知: %s\n", status)
	return false
}

// 清理WebView2旧安装残留
func cleanupWebView2Residues() {
	fmt.Println("正在清理可能的WebView2安装残留...")

	// 清理常见的安装路径
	pathsToClean := []string{
		filepath.Join(os.Getenv("ProgramFiles(x86)"), "Microsoft", "EdgeWebView"),
		filepath.Join(os.Getenv("ProgramFiles"), "Microsoft", "EdgeWebView"),
		filepath.Join(os.Getenv("LOCALAPPDATA"), "Microsoft", "EdgeWebView"),
	}

	for _, path := range pathsToClean {
		if _, err := os.Stat(path); err == nil {
			fmt.Printf("发现残留目录: %s，尝试清理...\n", path)
			err := os.RemoveAll(path)
			if err != nil {
				fmt.Printf("警告: 无法清理目录 %s: %v\n", path, err)
			} else {
				fmt.Printf("已成功清理目录: %s\n", path)
			}
		}
	}
}

// 安装WebView2 Runtime
func installWebView2Runtime(installerPath string) error {
	fmt.Println("正在安装WebView2 Runtime...")
	fmt.Println("注意: 安装过程可能需要3-5分钟时间，请耐心等待...")
	fmt.Println("(如果安装长时间无响应，可能需要以管理员身份重新运行程序)")

	// 检测是否在虚拟机环境中
	isVirtualMachine := false
	vmIndicators := []string{"VMware", "VirtualBox", "Hyper-V", "QEMU", "Parallels"}
	for _, indicator := range vmIndicators {
		if vmCheck, _ := exec.Command("wmic", "computersystem", "get", "model").Output(); strings.Contains(string(vmCheck), indicator) {
			isVirtualMachine = true
			break
		}
	}

	if isVirtualMachine {
		fmt.Println("检测到虚拟机环境，正在进行特殊优化...")
		// 虚拟机环境下的特殊处理
		// 1. 检查并修复Windows Installer服务
		checkWindowsInstallerService()

		// 2. 清理可能的旧安装残留
		cleanupWebView2Residues()

		// 3. 提示增加虚拟机资源
		fmt.Println("建议: 确保虚拟机分配了至少4GB内存和2个CPU核心，以确保WebView2正常安装和运行")
		time.Sleep(3 * time.Second)
	}

	// 创建一个上下文，设置8分钟的超时（虚拟机环境下增加超时时间）
	timeout := 5 * time.Minute
	if isVirtualMachine {
		timeout = 8 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// 创建日志文件以捕获安装输出
	logFile, err := os.Create(filepath.Join(os.TempDir(), "WebView2InstallLog.txt"))
	if err != nil {
		fmt.Printf("警告: 无法创建安装日志文件: %v\n", err)
	} else {
		defer logFile.Close()
	}

	// 使用更全面的静默安装参数组合
	// 尝试不同的参数组合以提高兼容性
	params := []string{"/silent", "/install", "/norestart"}
	if isVirtualMachine {
		// 虚拟机环境下使用更保守的参数
		params = []string{"/passive", "/install", "/norestart"}
	}
	cmd := exec.CommandContext(ctx, installerPath, params...)

	// 捕获标准输出和错误输出
	if logFile != nil {
		cmd.Stdout = logFile
		cmd.Stderr = logFile
	}

	// 启动安装进程
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动安装程序失败: %v\n请尝试手动下载安装WebView2 Runtime", err)
	}

	// 显示进度指示器和倒计时
	done := make(chan struct{})
	go func() {
		const chars = "/-\\|"
		startTime := time.Now()
		for i := 0; ; i++ {
			select {
			case <-done:
				fmt.Printf("\r安装完成!               \n")
				return
			case <-time.After(200 * time.Millisecond):
				elapsed := time.Since(startTime).Minutes()
				fmt.Printf("\r安装中 %c (已用时: %.1f分钟)", chars[i%len(chars)], elapsed)
			}
		}
	}()

	// 等待安装完成或超时
	err = cmd.Wait()
	close(done)

	if ctx.Err() == context.DeadlineExceeded {
		logPath := filepath.Join(os.TempDir(), "WebView2InstallLog.txt")
		return fmt.Errorf("安装超时(%d分钟)，请尝试以下解决方法:\n1. 以管理员身份重新运行程序\n2. 手动下载安装: https://developer.microsoft.com/zh-cn/microsoft-edge/webview2/\n3. 查看安装日志: %s\n4. 在虚拟机环境中，请确保分配足够的资源(至少4GB内存，2个CPU核心)", int(timeout.Minutes()), logPath)
	}

	if err != nil {
		// 安装失败，尝试使用备用参数
		fmt.Println("\n首次安装失败，尝试使用备用安装参数...")
		logPath := filepath.Join(os.TempDir(), "WebView2InstallLog.txt")
		return fmt.Errorf("安装失败，请尝试以下解决方法:\n1. 以管理员身份重新运行程序\n2. 手动下载安装: https://developer.microsoft.com/zh-cn/microsoft-edge/webview2/\n3. 查看安装日志: %s\n4. 清理旧安装残留后重试\n错误信息: %v", logPath, err)
	}

	// 安装完成后再次检查是否安装成功
	if !checkWebView2Runtime() {
		return fmt.Errorf("安装程序已完成，但未检测到WebView2 Runtime，请手动安装")
	}

	fmt.Println("WebView2 Runtime安装成功!")
	return nil
}

func main() {
	// 设置DPI感知（Windows特有）
	runtime.LockOSThread()

	// 检查WebView2 Runtime是否已安装
	if !checkWebView2Runtime() {
		// WebView2 Runtime未安装，提供自动安装选项
		fmt.Println("错误: 未检测到Microsoft Edge WebView2 Runtime")
		fmt.Println("是否自动下载并安装WebView2 Runtime？(y/n): ")
		var choice string
		fmt.Scanln(&choice)

		if strings.ToLower(choice) == "y" {
			// 创建临时目录
			tempDir := filepath.Join(os.TempDir(), "WebCut_WebView2")
			err := os.MkdirAll(tempDir, 0755)
			if err != nil {
				fmt.Printf("无法创建临时目录: %v\n", err)
				fmt.Println("\n按任意键退出...")
				fmt.Scanln()
				return
			}

			// 安装包路径
			installerPath := filepath.Join(tempDir, "WebView2RuntimeInstaller.exe")

			// 下载安装包
			err = downloadWebView2Runtime(installerPath)
			if err != nil {
				fmt.Printf("%v\n", err)
				fmt.Println("\n按任意键退出...")
				fmt.Scanln()
				return
			}

			// 安装WebView2 Runtime
			err = installWebView2Runtime(installerPath)
			if err != nil {
				fmt.Printf("%v\n", err)
				fmt.Println("您可以手动下载并安装: https://go.microsoft.com/fwlink/p/?LinkId=2124703")
				fmt.Println("\n按任意键退出...")
				fmt.Scanln()
				return
			}

			// 清理临时文件
			err = os.RemoveAll(tempDir)
			if err != nil {
				fmt.Printf("清理临时文件时出错: %v\n", err)
			}

			fmt.Println("\n请重新运行程序以启动WebCut。")
			fmt.Println("\n按任意键退出...")
			fmt.Scanln()
			return
		} else {
			// 用户选择不自动安装
			fmt.Println("解决方案:")
			fmt.Println("1. 下载并安装WebView2 Runtime: https://go.microsoft.com/fwlink/p/?LinkId=2124703")
			fmt.Println("2. 安装完成后，重新运行此程序")
			fmt.Println("")
			fmt.Println("按任意键退出...")
			fmt.Scanln()
			return
		}
	}

	// 创建并启动本地HTTP服务器
	serverAddr = startServer()

	// 创建WebView窗口
	w := webview2.NewWithOptions(webview2.WebViewOptions{
		Debug: true,
	})
	defer w.Destroy()

	// 设置窗口标题
	w.SetTitle("WebCut-网页快照")

	// 加载本地服务器的HTML页面
	w.Navigate(fmt.Sprintf("http://%s", serverAddr))

	// 运行WebView主循环
	w.Run()
}

// 启动本地HTTP服务器
type PageData struct {
	ServerAddr string
}

func startServer() string {
	// 创建一个监听器，使用固定端口1425
	listener, err := net.Listen("tcp", "127.0.0.1:1425")
	if err != nil {
		panic(err)
	}

	// 获取分配的地址和端口
	addr := "127.0.0.1:1425"

	// 定义HTML模板
	htmlTemplate := `
<!DOCTYPE html>
<html lang="zh-CN">
<head>
	<meta charset="UTF-8">
	<meta name="viewport" content="width=device-width, initial-scale=1.0">
	<title>URL截图查看器</title>
	<style>
		body {
			font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, Oxygen, Ubuntu, Cantarell, sans-serif;
			max-width: 1000px;
			margin: 0 auto;
			padding: 20px;
			background-color: #f5f5f5;
		}
		.container {
			background-color: white;
			border-radius: 8px;
			box-shadow: 0 2px 10px rgba(0,0,0,0.1);
			padding: 30px;
		}
		h1 {
			color: #333;
			text-align: center;
			margin-bottom: 30px;
		}
		h3 {
			color: #555;
			margin-top: 20px;
			margin-bottom: 15px;
		}
		.form-group {
			margin-bottom: 20px;
		}
		label {
			display: block;
			margin-bottom: 8px;
			font-weight: 500;
			color: #555;
		}
		input[type="text"], select {
			width: 100%;
			padding: 10px;
			border: 1px solid #ddd;
			border-radius: 4px;
			font-size: 16px;
		}
		button {
			background-color: #4CAF50;
			color: white;
			border: none;
			padding: 12px 24px;
			border-radius: 4px;
			font-size: 16px;
			cursor: pointer;
			margin-right: 10px;
		}
		button:hover {
			background-color: #45a049;
		}
		.button-group {
			display: flex;
			justify-content: center;
			flex-wrap: wrap;
			margin: 20px 0;
		}
		.url-list-container {
			margin: 20px 0;
			padding: 15px;
			border: 1px solid #ddd;
			border-radius: 4px;
			background-color: #f9f9f9;
			display: none;
		}
		.url-list-container.show {
			display: block;
		}
		.url-list-title {
			margin-top: 0;
			margin-bottom: 10px;
			font-size: 16px;
			color: #333;
		}
		.url-list {
			max-height: 200px;
			overflow-y: auto;
			padding-left: 20px;
		}
		.url-list li {
			margin-bottom: 5px;
			font-size: 14px;
			color: #555;
		}
		.preview-container {
			margin-top: 20px;
		}
		.single-preview {
			text-align: center;
		}
		#screenshotPreview {
			max-width: 100%;
			max-height: 400px;
			border: 1px solid #ddd;
			border-radius: 4px;
		}
		.batch-previews {
			max-height: 500px;
			overflow-y: auto;
			border: 1px solid #ddd;
			border-radius: 4px;
			padding: 15px;
		}
		.preview-grid {
			display: grid;
			grid-template-columns: repeat(auto-fill, minmax(300px, 1fr));
			gap: 15px;
		}
		.preview-item {
			border: 1px solid #eee;
			border-radius: 4px;
			padding: 10px;
			background-color: #fafafa;
		}
		.preview-item.error {
			border-color: #f5c6cb;
			background-color: #f8d7da;
		}
		.preview-item img {
			max-width: 100%;
		height: auto;
			border-radius: 4px;
		}
		.error-message {
			color: #721c24;
			font-weight: bold;
			margin-bottom: 5px;
		}
		.error-detail {
			color: #721c24;
			font-size: 12px;
			margin-top: 5px;
			text-align: left;
			word-break: break-word;
		}
		.preview-item .url {
			margin-top: 8px;
			font-size: 14px;
			color: #555;
			word-break: break-all;
		}
		.message {
			margin-top: 20px;
			padding: 15px;
			border-radius: 4px;
			text-align: center;
		}
		.success {
			background-color: #d4edda;
			color: #155724;
			border: 1px solid #c3e6cb;
		}
		.error {
			background-color: #f8d7da;
			color: #721c24;
			border: 1px solid #f5c6cb;
		}
		.loading {
			margin: 20px auto;
			width: 40px;
			height: 40px;
			border: 4px solid #f3f3f3;
			border-top: 4px solid #4CAF50;
			border-radius: 50%;
			animation: spin 1s linear infinite;
			display: none;
		}
		.progress-container {
			margin: 20px auto;
			width: 80%;
			display: none;
		}
		.progress-bar {
			width: 100%;
			height: 20px;
			background-color: #f3f3f3;
			border-radius: 10px;
			overflow: hidden;
		}
		.progress-fill {
			height: 100%;
			background-color: #4CAF50;
			width: 0%;
			transition: width 0.3s ease;
		}
		.progress-text {
			text-align: center;
			margin-top: 5px;
			font-size: 14px;
			color: #666;
		}
		@keyframes spin {
			0% { transform: rotate(0deg); }
			100% { transform: rotate(360deg); }
		}
	</style>
</head>
<body>
	<div class="container">
		<h1>WebCut-网页快照</h1>
		
		<div class="form-group">
			<label for="urlInput">输入URL:</label>
			<input type="text" id="urlInput" placeholder="https://www.example.com">
		</div>
		

		
		<div class="form-group">
			<label for="fullPageSelect">截图范围:</label>
			<select id="fullPageSelect">
				<option value="true" selected>整页</option>
				<option value="false">可见区域</option>
			</select>
		</div>

		<div class="form-group">
			<label for="useBatchCheckbox">
				<input type="checkbox" id="useBatchCheckbox">
				使用已加载的URL列表进行批量截图
			</label>
		</div>
		
		<div class="button-group">
				<button id="captureBtn">捕获截图</button>
				<button id="loadListBtn">加载URL列表</button>
				<input type="file" id="listFileInput" accept=".txt" style="display: none;">
			</div>
		
		<div id="urlListContainer" class="url-list-container">
			<h3 class="url-list-title">已加载的URL列表</h3>
			<ul id="urlListDisplay" class="url-list"></ul>
		</div>
		
		<div class="loading" id="loadingIndicator"></div>
		
		<div id="progress-container" class="progress-container">
			<div class="progress-bar">
				<div class="progress-fill"></div>
			</div>
			<div class="progress-text">已完成 0 / 0</div>
		</div>
		
		<div id="message" class="message" style="display: none;"></div>
		
		<div class="preview-container">
				<h3>截图结果</h3>
				<div id="batchPreviews" class="batch-previews" style="display: none;">
					<div id="previewGrid" class="preview-grid"></div>
				</div>
				<div id="singlePreview" class="single-preview">
					<img id="screenshotPreview" src="" alt="截图结果" style="display: none;">
				</div>
			</div>
	</div>

	<script>
		var captureBtn = document.getElementById('captureBtn');
		var urlInput = document.getElementById('urlInput');
		var qualitySelect = document.getElementById('qualitySelect');
		var fullPageSelect = document.getElementById('fullPageSelect');
		var screenshotPreview = document.getElementById('screenshotPreview');
		var loadingIndicator = document.getElementById('loadingIndicator');
		var message = document.getElementById('message');
		var loadListBtn = document.getElementById('loadListBtn');
		var listFileInput = document.getElementById('listFileInput');
		var batchPreviews = document.getElementById('batchPreviews');
		var singlePreview = document.getElementById('singlePreview');
		var previewGrid = document.getElementById('previewGrid');
		var progressContainer = document.getElementById('progress-container');
		var progressFill = document.querySelector('.progress-fill');
		var progressText = document.querySelector('.progress-text');
		var urlList = [];

		// 重置预览区域
		function resetPreviews() {
			batchPreviews.style.display = 'none';
			singlePreview.style.display = 'block';
			screenshotPreview.style.display = 'block';
			previewGrid.innerHTML = '';
		}

		// 更新进度条
		function updateProgress(current, total) {
			var percent = (current / total) * 100;
			progressFill.style.width = percent + '%';
			progressText.textContent = '已完成 ' + current + ' / ' + total;
		}

		var urlListContainer = document.getElementById('urlListContainer');
		var urlListDisplay = document.getElementById('urlListDisplay');
		var useBatchCheckbox = document.getElementById('useBatchCheckbox');

		// 显示消息
		function showMessage(text, isError) {
			if (isError === undefined) isError = false;
			message.textContent = text;
			message.className = 'message ' + (isError ? 'error' : 'success');
			message.style.display = 'block';
			setTimeout(function() {
				message.style.display = 'none';
			}, 3000);
		}

		// 显示URL列表
		function displayURLList(urls) {
			if (urls.length === 0) {
				urlListContainer.classList.remove('show');
				return;
			}

			urlListDisplay.innerHTML = '';
			urls.forEach(function(url, index) {
				var li = document.createElement('li');
				li.textContent = url;
				urlListDisplay.appendChild(li);
			});

			urlListContainer.classList.add('show');
		}

		// 捕获截图 - 支持单个和批量
		captureBtn.addEventListener('click', function() {
			// 如果勾选了使用批量并且有URL列表，则执行批量截图
			if (useBatchCheckbox.checked && urlList.length > 0) {
				performBatchCapture();
			} else {
				// 否则执行单个URL截图
				var url = urlInput.value.trim();
				if (!url) {
					showMessage('请输入URL', true);
					return;
				}

				if (!url.startsWith('http://') && !url.startsWith('https://')) {
					showMessage('URL必须以http://或https://开头', true);
					return;
				}

				loadingIndicator.style.display = 'block';

				try {
					var fullPage = fullPageSelect.value === 'true';
					
					fetch('/capture', {
						method: 'POST',
						headers: {
							'Content-Type': 'application/json'
						},
						body: JSON.stringify({
							url: url,
							fullPage: fullPage
						})
					}).then(function(response) {
						return response.json();
					}).then(function(data) {
						if (data.base64Image) {
							screenshotPreview.src = 'data:image/png;base64,' + data.base64Image;
							resetPreviews();
							showMessage('截图成功');
						} else {
							showMessage(data.error || '截图失败', true);
						}
					}).catch(function(error) {
						showMessage('截图失败: ' + error.message, true);
					}).finally(function() {
						loadingIndicator.style.display = 'none';
					});
				} catch (error) {
					showMessage('截图失败: ' + error.message, true);
					loadingIndicator.style.display = 'none';
				}
			}
		});

		// 执行批量截图
		function performBatchCapture() {
			if (urlList.length === 0) {
				showMessage('请先加载URL列表', true);
				return;
			}

			loadingIndicator.style.display = 'none';
			progressContainer.style.display = 'block';
			updateProgress(0, urlList.length);

			try {
				var fullPage = fullPageSelect.value === 'true';

				fetch('/batch-capture', {
					method: 'POST',
					headers: {
						'Content-Type': 'application/json'
					},
					body: JSON.stringify({
						urls: urlList,
						fullPage: fullPage
					})
				}).then(function(response) {
						// 设置流式读取器
						var reader = response.body.getReader();
						var decoder = new TextDecoder();
						var finalResult = null;

						return new Promise(function(resolve, reject) {
							function read() {
								reader.read().then(function(result) {
									if (result.done) {
										// 流式读取完成，使用/batch-result接口获取完整结果
										fetch('/batch-result').then(function(res) {
											return res.json();
										}).then(resolve).catch(reject);
										return;
									}

									try {
										var chunk = decoder.decode(result.value, { stream: false }); // 改为非流式解码，确保完整解码
										// 处理每个进度更新
										var lines = chunk.split('\n');
										lines.forEach(function(line) {
											if (line && line.trim()) {
												try {
													// 尝试解析JSON前先检查是否包含有效的JSON结构
													if (line.includes('{') && line.includes('}')) {
														var data = JSON.parse(line);
														if (data.progress !== undefined) {
															updateProgress(data.progress.current, data.progress.total);
														}
														// 检查是否是最终结果
														if (data.results) {
															finalResult = data;
														}
													}
												} catch (e) {
													// 静默处理错误，避免控制台大量报错
												}
											}
										});
									} catch (e) {
										// 静默处理解码错误
									}
									read();
								}).catch(reject);
							}
							read();
						});
				}).then(function(data) {
						if (data.results) {
							// 清空预览网格
							previewGrid.innerHTML = '';
							// 生成预览内容
							data.results.forEach(function(result) {
								if (result.base64Image) {
									var item = document.createElement('div');
									item.className = 'preview-item';
									item.innerHTML = '<img src="data:image/png;base64,' + result.base64Image + '" alt="' + result.url + '"><div class="url">' + result.url + '</div>';
									previewGrid.appendChild(item);
								} else if (result.error) {
									// 显示失败的URL和原因
									var item = document.createElement('div');
									item.className = 'preview-item error';
									item.innerHTML = '<div class="error-message">截图失败</div><div class="url">' + result.url + '</div><div class="error-detail">' + result.error + '</div>';
									previewGrid.appendChild(item);
								}
							});

							// 设置预览区域显示模式，不调用resetPreviews()避免清空内容
							batchPreviews.style.display = 'block';
							singlePreview.style.display = 'none';
							screenshotPreview.style.display = 'none';
							
							// 显示完成消息
							showMessage('批量截图完成，成功 ' + data.successCount + ' 个，失败 ' + data.failureCount + ' 个');
						} else {
							showMessage(data.error || '批量截图失败', true);
						}
					}).catch(function(error) {
						showMessage('批量截图失败: ' + error.message, true);
					}).finally(function() {
						loadingIndicator.style.display = 'none';
						progressContainer.style.display = 'none';
					});
				} catch (error) {
					showMessage('批量截图失败: ' + error.message, true);
					loadingIndicator.style.display = 'none';
					progressContainer.style.display = 'none';
				}
			}

		// 加载URL列表
		loadListBtn.addEventListener('click', function() {
			listFileInput.click();
		});

		// 处理文件选择
		listFileInput.addEventListener('change', function() {
			var file = listFileInput.files[0];
			if (!file) {
				return;
			}

			var reader = new FileReader();
			reader.onload = function(e) {
				var content = e.target.result;
				// 按行分割，过滤空行和注释行
				urlList = content.split('\n')
					.map(function(line) { return line.trim(); })
					.filter(function(line) {
						return line && !line.startsWith('#');
					});

				showMessage('成功加载 ' + urlList.length + ' 个URL');
				displayURLList(urlList);
			};
			reader.readAsText(file);
			// 重置文件输入，允许重复选择同一个文件
			listFileInput.value = '';
		});



		// 移除了保存功能，直接在捕获后显示截图结果
	</script>
</body>
</html>`

	// 处理根路径请求
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		tmpl := template.Must(template.New("page").Parse(htmlTemplate))
		tmpl.Execute(w, PageData{ServerAddr: addr})
	})

	// 处理截图请求
	http.HandleFunc("/capture", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// 读取请求体
		body, err := ioutil.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "Failed to read request body", http.StatusBadRequest)
			return
		}

		// 解析JSON请求
		var req struct {
			URL      string `json:"url"`
			FullPage bool   `json:"fullPage"`
		}

		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "Invalid JSON format", http.StatusBadRequest)
			return
		}

		// 捕获截图
		imgData, err := captureScreenshot(req.URL, req.FullPage, 30)
		if err != nil {
			json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf("截图失败: %v", err)})
			return
		}

		// 保存当前截图
		currentScreenshot = imgData

		// 将截图转换为base64并返回
		base64Image := base64.StdEncoding.EncodeToString(imgData)
		json.NewEncoder(w).Encode(map[string]string{"base64Image": base64Image})
	})

	// 处理批量截图请求
	var batchResults []map[string]interface{}
	var batchSuccessCount, batchFailureCount int
	var batchResultMutex sync.Mutex

	// 并发批量截图处理
	http.HandleFunc("/batch-capture", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// 设置响应头以支持流式传输
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Transfer-Encoding", "chunked")
		w.Header().Set("X-Content-Type-Options", "nosniff")

		// 读取请求体
		body, err := ioutil.ReadAll(r.Body)
		if err != nil {
			fmt.Fprintf(w, "{\"error\": \"Failed to read request body\"}\n")
			return
		}

		// 解析JSON请求
		var req struct {
			URLs     []string `json:"urls"`
			FullPage bool     `json:"fullPage"`
		}

		if err := json.Unmarshal(body, &req); err != nil {
			fmt.Fprintf(w, "{\"error\": \"Invalid JSON format\"}\n")
			return
		}

		// 清空之前的结果
		batchResultMutex.Lock()
		batchResults = []map[string]interface{}{}
		batchSuccessCount = 0
		batchFailureCount = 0
		batchResultMutex.Unlock()

		// 创建一个新的无头Chrome实例配置 - 添加忽略证书错误的选项
		opts := append(chromedp.DefaultExecAllocatorOptions[:],
			chromedp.Flag("headless", true),
			chromedp.Flag("disable-gpu", true),
			// 禁用不必要的功能以提高性能
			chromedp.Flag("disable-extensions", true),
			chromedp.Flag("disable-plugins", true),
			chromedp.Flag("disable-features", "TranslateUI,BlinkGenPropertyTrees"),
			// 忽略证书错误，允许访问HTTPS页面
			chromedp.Flag("ignore-certificate-errors", true),
			chromedp.Flag("allow-insecure-localhost", true),
			chromedp.Flag("accept-insecure-certs", true),
			chromedp.WindowSize(1280, 800), // 固定窗口大小提高性能
		)

		// 批量处理URL - 并发版本
		totalURLs := len(req.URLs)
		completedCount := 0
		completedCountMutex := sync.Mutex{}

		// 设置最大并发数，降低并发数以避免资源竞争
		maxConcurrency := 3
		semaphore := make(chan struct{}, maxConcurrency)
		resultChan := make(chan map[string]interface{}, totalURLs)
		var wg sync.WaitGroup

		// 启动进度更新goroutine
		progressTicker := time.NewTicker(500 * time.Millisecond)
		defer progressTicker.Stop()
		doneProgress := make(chan struct{})
		go func() {
			for {
				select {
				case <-progressTicker.C:
					completedCountMutex.Lock()
					progress := map[string]interface{}{
						"progress": map[string]int{
							"current": completedCount,
							"total":   totalURLs,
						},
					}
					completedCountMutex.Unlock()
					progressJSON, _ := json.Marshal(progress)
					fmt.Fprintf(w, "%s\n", string(progressJSON))
					if flusher, ok := w.(http.Flusher); ok {
						flusher.Flush()
					}
				case <-doneProgress:
					return
				}
			}
		}()

		// 启动并发任务
		for _, url := range req.URLs {
			wg.Add(1)
			semaphore <- struct{}{} // 获取信号量
			go func(url string) {
				defer wg.Done()
				defer func() { <-semaphore }() // 释放信号量

				// 为每个URL创建独立的执行分配器，避免资源竞争
				_, cancel := chromedp.NewExecAllocator(context.Background(), opts...)
				defer cancel()

				// 捕获截图 - 增加超时时间到60秒
				imgData, err := captureScreenshot(url, req.FullPage, 60)

				// 准备结果
				result := make(map[string]interface{})
				result["url"] = url
				// 记录日志，便于调试
				if err != nil {
					fmt.Printf("URL %s 截图失败: %v\n", url, err)
				} else {
					fmt.Printf("URL %s 截图成功\n", url)
				}
				if err != nil {
					result["error"] = err.Error()
					batchResultMutex.Lock()
					batchFailureCount++
					batchResultMutex.Unlock()
				} else {
					// 将截图转换为base64
					base64Image := base64.StdEncoding.EncodeToString(imgData)
					result["base64Image"] = base64Image

					// 保存到批量截图映射
					batchMutex.Lock()
					batchScreenshots[url] = imgData
					batchMutex.Unlock()

					batchResultMutex.Lock()
					batchSuccessCount++
					batchResultMutex.Unlock()
				}

				// 发送结果
				resultChan <- result

				// 更新完成计数
				completedCountMutex.Lock()
				completedCount++
				completedCountMutex.Unlock()
			}(url)
		}

		// 等待所有任务完成并收集结果
		go func() {
			wg.Wait()
			close(resultChan)
			close(doneProgress) // 停止进度更新
		}()

		// 收集所有结果
		for result := range resultChan {
			batchResultMutex.Lock()
			batchResults = append(batchResults, result)
			batchResultMutex.Unlock()
		}

		// 确保最后进度显示100%
		progress := map[string]interface{}{
			"progress": map[string]int{
				"current": totalURLs,
				"total":   totalURLs,
			},
		}
		progressJSON, _ := json.Marshal(progress)
		fmt.Fprintf(w, "%s\n", string(progressJSON))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}

		// 发送最终结果
		finalResult := map[string]interface{}{
			"results":      batchResults,
			"successCount": batchSuccessCount,
			"failureCount": batchFailureCount,
			"totalURLs":    totalURLs,
		}
		finalResultJSON, _ := json.Marshal(finalResult)
		fmt.Fprintf(w, "%s\n", string(finalResultJSON))
	})

	// 获取批量截图结果
	http.HandleFunc("/batch-result", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		batchResultMutex.Lock()
		defer batchResultMutex.Unlock()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"results":      batchResults,
			"successCount": batchSuccessCount,
			"failureCount": batchFailureCount,
			"totalURLs":    len(batchResults),
		})
	})

	// 批量保存功能已移除

	// 在后台启动服务器
	go http.Serve(listener, nil)

	return addr
}

// captureScreenshot 捕获指定URL的截图
func captureScreenshot(url string, fullPage bool, timeoutSec int) ([]byte, error) {

	// 创建一个新的无头Chrome实例 - 添加忽略证书错误的选项
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", true),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("ignore-certificate-errors", true),
		chromedp.Flag("allow-insecure-localhost", true),
	)

	// 创建执行分配器
	allocCtx, cancel := chromedp.NewExecAllocator(context.Background(), opts...)
	defer cancel()

	// 创建新的上下文
	ctx, cancel := chromedp.NewContext(allocCtx)
	defer cancel()

	// 设置超时
	ctx, cancel = context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer cancel()

	// 存储截图结果
	var buf []byte

	// 运行任务：导航到URL并截图
	err := chromedp.Run(ctx,
		chromedp.Navigate(url),
		// 等待页面加载完成
		chromedp.WaitVisible(`body`, chromedp.ByQuery),
		// 等待一段时间确保JS渲染完成
		chromedp.Sleep(2*time.Second),
		// 根据参数选择截图方式
		chromedp.ActionFunc(func(ctx context.Context) error {
			if fullPage {
				// 使用默认质量参数
				return chromedp.FullScreenshot(&buf, 90).Do(ctx)
			} else {
				// 捕获可见区域截图
				return chromedp.CaptureScreenshot(&buf).Do(ctx)
			}
		}),
	)

	if err != nil {
		return nil, fmt.Errorf("执行截图任务失败: %v", err)
	}

	return buf, nil
}
