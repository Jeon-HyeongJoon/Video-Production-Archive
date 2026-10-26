# 05. 실행, 자원과 배포 설계

## 1. Step 자원 분류

| 분류 | 예 | 기본 동시성 |
|---|---|---:|
| heavy | render, STT, 대용량 checksum/copy | Worker마다 1 |
| light | ffprobe, DB 기록, 작은 thumbnail | Worker마다 2 |
| control | HTTP, 승인, 상태 조회 | heavy slot 비사용 |

초기에는 render와 STT를 동시에 실행하지 않는다. 처리량 부족이 실제 queue 측정으로 확인되기 전에는 동시성을 높이지 않는다.

## 2. 자원 예약 순서

```text
1. ExecutionPriority.IMMEDIATE Step 존재 여부(heavy만 적용)
2. 예상 메모리 + 6GB 안전 여유
3. scratch 예상량 + 안전 여유
4. 물리 장치 lock
5. heavy/light semaphore
6. BACKGROUND idle hysteresis(heavy만 적용)
7. ExecutionLease 발급
```

안전 조건은 `RES-003~007`로 검증한다. 예약 이후 실제 사용량이 예상치를 지속적으로 넘으면 새 lease 발급을 중단하고 경고 Event를 생성한다.

## 3. 유휴 판정

최초 background 시작 조건:

```text
5분 평균 CPU < 35%
5분 평균 GPU < 20%
idle 조건 5분 유지
IMMEDIATE Step 없음
```

사용자 부하 후 재개는 10분 안정 상태를 요구한다. `RES-001~002`

IMMEDIATE Step은 idle 조건을 우회하지만 메모리, scratch, disk lock과 Worker heavy slot은 우회하지 않는다. light Step은 idle과 PRIORITY gate를 적용하지 않되 메모리와 light slot을 지킨다. `RES-003`, `RES-008`

## 4. 실행 중 부하 변화

1. 신규 BACKGROUND ExecutionLease 발급 중단
2. Worker/subprocess를 Below Normal priority로 유지
3. CPU thread 수 제한
4. 사용자 작업을 계속 방해하면 검증된 안전 지점에서 중단
5. 임시 출력과 resume 정보 기록 후 StepState.WAITING + WaitReason.IDLE로 반환

일반 FFmpeg 프로세스를 장시간 OS suspend하지 않는다. 재개가 필요한 작업은 segment 단위 출력과 최종 병합처럼 명시적인 checkpoint를 가진 profile로 만든다.

## 5. Subprocess 계약

모든 실행기는 다음 공통 인터페이스를 구현한다.

```text
Prepare(input IDs, profile) → immutable execution plan
Start(context, plan) → process handle
Observe(handle) → progress/log/metrics
Cancel(handle) → graceful stop, timeout 후 kill
Verify(temp outputs) → inspection/checksum/QC evidence
Publish(evidence) → artifact IDs
```

명령행은 배열 인수로 만들고 shell 문자열 결합을 사용하지 않는다. 출력은 임시 경로에서 검증한 뒤에만 게시한다. 로그에는 원문 credential이나 민감 경로를 노출하지 않는다. Core 계약은 `EXE-001~002`, DB와 파일 게시 사이 재시작 조정은 `EXE-003` integration test가 검증한다.

## 6. Windows 서비스 배포

- `video-archive-control`과 `video-archive-worker`를 별도 Windows Service로 등록
- 자동 시작은 delayed start 사용
- 서비스 계정에 필요한 storage root와 Secret Store 권한만 부여
- 화면 잠금과 무관하게 서비스 지속
- AC 전원 시 sleep/hibernate 비활성화
- 정전 후 자동 부팅과 UPS 종료 hook 구성
- 로그와 DB는 scratch 및 외장 archive와 분리

네트워크 설정은 서비스 설정과 Windows Firewall에 이중으로 둔다.

```text
listen_address: 사무실 NIC의 고정 내부 IP
allowed_cidrs: 실제 사무실 subnet만 지정
public_profile_inbound: deny
port_forwarding/upnp: disabled
mcp_listen_address: 127.0.0.1  # auth disabled인 경우
```

Tailscale 같은 overlay network를 사용할 때도 전체 CGNAT 대역을 허용하기보다 해당 장치/사용자 ACL과 필요한 주소만 허용한다.

원격 Worker PC에는 `video-archive-worker`만 설치한다. Worker가 Control로 outbound 연결하므로 inbound 포트를 열지 않으며, Worker ID와 credential reference는 양쪽 로컬 설정에 사전 등록한다. Dashboard의 사용자 인증 비활성 설정은 Worker service 인증을 끄지 않는다.

## 7. 종료와 재시작

Control 종료:

1. 새 Command/lease 수락 중단
2. Notification feed에 shutdown 예정 알림
3. DB transaction 완료
4. Worker lease는 만료 복구 가능 상태로 둠

Worker 종료:

1. 새 lease claim 중단
2. 안전 종료 가능한 subprocess에 취소 전달
3. 마지막 heartbeat와 중단 이유 보고
4. timeout 후 프로세스 트리 종료

재시작 시 `WFL-004`, `WFL-008~009`의 ExecutionLease와 publish 복구 규칙을 적용한다.

## 8. 관측성

- 구조화 로그: job_id, step_id, command_id, worker_id, request_id
- metric: queue depth, wait reason, CPU/GPU/memory, disk throughput, failure rate
- Event: 상태 전이, 승인, fallback, device connect/disconnect
- Notification: Event 또는 metric에서 만든 실시간 메시지. Event 기반 알림만 재연결 replay
- process 원문 로그: 회전 파일, DB에는 위치와 tail만 저장

Provider 관련 필드는 저장 전 `AGT-008` 허용 목록 정책으로 정제한다. feed의 replay 분리는 `NTF-001`, 실제 HTTP/SSE 재연결은 `NTF-002` integration test가 검증한다.

도메인 기능 실행은 Capability Executor의 공통 Evidence Sink가 시작·진행·완료·실패와 상관 ID를 기록한다. requirement test는 CI evidence runner가 구조화한 뒤 CI artifact로 누적한다. 로컬 테스트 실행은 완료 판정 증적으로 사용하지 않는다. 운영 증적과 테스트 증적은 형식을 공유할 수 있지만 저장 수명주기와 원장은 분리한다. 자세한 계약은 [10. 도메인 기능 실행과 검증 증적](10-capabilities-observability.md)을 따른다. `OBS-001`, `TST-001`
