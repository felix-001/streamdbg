# rtptool

## 准备
wireshark抓到mpeg ps over rtp的包，分析 -> 追踪流 -> tcp流 -> 原始数据 -> 另存为，可以把tcp的负载dump出来，都是rtp的包。

## 说明
- -csv-file  
将每一包的rtp信息保存为csv

- -file  
输入文件，tcp的负载

- -output-file  
输出文件，为mpeg ps包，保存为xxx.mpg

- -remote-addr  
接收rtp包的流媒体服务器地址，例如127.0.0.1:9001

- -search-bytes  
搜索一个字节序列，打印出所在的序列号，时间戳等信息

- -show-progress  
显示进度条

-  -Verbose  
显示更详细的信息

## todo
改名rtptool
