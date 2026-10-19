package main

// Кнопки, которые нажимает абонент GSM.
//
// В VoLTE цифры приходят из сети пакетами telephone-event (RFC 4733), их
// съедает IMS в модеме, поэтому в PCM тона нет. Но модем всё равно сообщает
// о каждом нажатии индикацией QMI_VOICE_DTMF_IND (события 8/9 — начало и
// конец входящей цифры) всем клиентам QMI VOICE, кто подписан на DTMF.
// На этой прошивке их никто не обрабатывает: imsrcsd такие события
// выкидывает, qcril на них вообще не подписан. Подписываемся сами через
// IPC router и пишем в лог строки
//
//	DTMF start 5
//	DTMF stop 5
//
// а приложение уже шлёт их в SIP как RFC 4733.

import (
	"context"
	"encoding/binary"
	"syscall"
	"unsafe"
)

const (
	afMsmIpc     = 27
	msmIpcAddrID = 2
	// IPC_ROUTER_IOCTL_LOOKUP_SERVER = _IOWR(0xC3, 2, struct sockaddr_msm_ipc)
	ipcLookupServer = 0xC014C302

	svcVoice = 9

	qmiRequest    = 0
	qmiResponse   = 2
	qmiIndication = 4

	msgIndicationRegister = 0x0003
	msgDtmfInd            = 0x002b

	dtmfStart = 0x08 // DTMF_EVENT_IP_INCOMING_DTMF_START
	dtmfStop  = 0x09 // DTMF_EVENT_IP_INCOMING_DTMF_STOP
)

// В /vendor/etc/sec_config для голосового сервиса стоит
// "9:4294967295:1001:3004": пускают только radio и net_raw.
// Без этих групп запросы молча пропадают, даже от root.
var voiceGroups = []int{1001, 3004}

func watchDtmf(ctx context.Context) {
	if err := syscall.Setgroups(voiceGroups); err != nil {
		logf("DTMF off: setgroups: %v", err)
		return
	}
	fd, err := syscall.Socket(afMsmIpc, syscall.SOCK_DGRAM, 0)
	if err != nil {
		logf("DTMF off: socket: %v", err)
		return
	}
	defer syscall.Close(fd)

	node, port, err := findVoice(fd)
	if err != nil {
		logf("DTMF off: нет голосового сервиса: %v", err)
		return
	}

	// Заголовок QMI (флаг, txn, id, длина) и TLV 0x10 reg_dtmf_events = 1.
	req := []byte{qmiRequest, 1, 0, msgIndicationRegister, 0, 4, 0, 0x10, 1, 0, 1}
	if err := send(fd, node, port, req); err != nil {
		logf("DTMF off: register: %v", err)
		return
	}

	// Таймаут нужен, чтобы раз в секунду проверять ctx.
	tv := syscall.Timeval{Sec: 1}
	syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &tv)

	buf := make([]byte, 4096)
	for ctx.Err() == nil {
		n, err := recv(fd, buf)
		if err != nil || n < 7 {
			continue
		}
		kind := buf[0]
		id := binary.LittleEndian.Uint16(buf[3:])
		body := buf[7:n]

		if kind == qmiResponse && id == msgIndicationRegister {
			res := findTLV(body, 0x02)
			if len(res) < 4 || binary.LittleEndian.Uint16(res) != 0 {
				logf("DTMF off: подписку не приняли: % x", res)
				return
			}
			logf("DTMF ready (voice %d:%#x)", node, port)
			continue
		}
		if kind != qmiIndication || id != msgDtmfInd {
			continue
		}

		// dtmf_info: call_id, событие, длина, цифры
		info := findTLV(body, 0x01)
		if len(info) < 4 || info[2] == 0 {
			continue
		}
		switch info[1] {
		case dtmfStart:
			logf("DTMF start %c", info[3])
		case dtmfStop:
			logf("DTMF stop %c", info[3])
		}
	}
}

// findVoice ищет сервер QMI VOICE. Место в запросе — под одну запись,
// instance и маска нулевые, то есть подойдёт любой.
func findVoice(fd int) (node, port uint32, err error) {
	var args [20 + 16]byte // server_lookup_args + один msm_ipc_server_info
	binary.LittleEndian.PutUint32(args[0:], svcVoice)
	binary.LittleEndian.PutUint32(args[8:], 1) // num_entries_in_array
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), ipcLookupServer, uintptr(unsafe.Pointer(&args[0])))
	if e != 0 {
		return 0, 0, e
	}
	if int32(binary.LittleEndian.Uint32(args[12:])) < 1 {
		return 0, 0, syscall.ENOENT
	}
	node = binary.LittleEndian.Uint32(args[20:])
	port = binary.LittleEndian.Uint32(args[24:])
	return node, port, nil
}

// send отправляет сообщение на node:port. sockaddr_msm_ipc: family,
// addrtype по смещению 4, адрес порта с 8.
func send(fd int, node, port uint32, msg []byte) error {
	var addr [20]byte
	binary.LittleEndian.PutUint16(addr[0:], afMsmIpc)
	addr[4] = msmIpcAddrID
	binary.LittleEndian.PutUint32(addr[8:], node)
	binary.LittleEndian.PutUint32(addr[12:], port)
	_, _, e := syscall.Syscall6(syscall.SYS_SENDTO, uintptr(fd),
		uintptr(unsafe.Pointer(&msg[0])), uintptr(len(msg)), 0,
		uintptr(unsafe.Pointer(&addr[0])), uintptr(len(addr)))
	if e != 0 {
		return e
	}
	return nil
}

// recv — recvfrom без адреса отправителя. syscall.Recvfrom тут не годится:
// семейство AF_MSM_IPC он разобрать не может и возвращает ошибку вместо данных.
func recv(fd int, buf []byte) (int, error) {
	n, _, e := syscall.Syscall6(syscall.SYS_RECVFROM, uintptr(fd),
		uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), 0, 0, 0)
	if e != 0 {
		return 0, e
	}
	return int(n), nil
}

func findTLV(b []byte, typ byte) []byte {
	for len(b) >= 3 {
		size := int(binary.LittleEndian.Uint16(b[1:]))
		if 3+size > len(b) {
			return nil
		}
		if b[0] == typ {
			return b[3 : 3+size]
		}
		b = b[3+size:]
	}
	return nil
}
