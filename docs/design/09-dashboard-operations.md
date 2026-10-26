# 09. Dashboard 운영 투영 설계

## 1. 목적

Dashboard의 첫 책임은 에이전트가 관리하는 대용량 파일 Job의 현재 업무 현황을 사람이 빠르게 파악하게 하는 것이다. 아카이브 검색 결과를 보여주는 화면이나 로컬 설정 파일을 편집하는 관리 도구와 구분한다.

초기 Dashboard는 다음 질문에 바로 답해야 한다.

- 에이전트가 지금 어떤 Job과 Step을 다루고 있는가
- 어떤 도구 호출 또는 Worker 실행이 진행 중인가
- 왜 대기하거나 실패했는가
- 어느 Worker와 저장소 자원이 병목인가
- 사람이 지금 개입해야 하는가

매체 소유권, 대여, 반환 승인과 반환 이력은 Dashboard와 도메인 모델의 범위가 아니다. 작업 공간 드라이브의 lock과 안전 해제 상태는 Job 실행 상태이므로 표시한다.

## 2. 읽기 모델

작업함의 각 행은 최소한 다음 값을 투영한다.

```text
job_id, template_name, target_title
control_mode
current_step_id, step_state, progress
executor_kind, actor_id, worker_id
agent_action, tool_call_status
wait_reason 또는 error_class
last_event_at, log_reference
```

작업 상세는 Step DAG, CommandRecord, Event, Approval, Agent Tool Call, Worker progress와 정제된 process log 참조를 시간순으로 결합한다. Dashboard가 이 기록의 원장이 되어서는 안 되며, SQLite 원장과 일시 metric에서 다시 만들 수 있는 읽기 모델이어야 한다.

Worker 상태는 CPU, memory, GPU/VRAM, scratch 여유, heavy slot, 접근 가능한 Storage와 heartbeat를 보여준다. 장치 정보는 Job을 실행할 수 있는지, PhysicalDevice 작업 공간의 lock 충돌과 안전 해제 가능 여부를 설명하는 범위에서만 보여주며 매체 반환 상태를 만들지 않는다.

요구사항: `DSH-001`

## 3. 사람 개입

Dashboard의 변경 동작은 업무 현황에서 출발한 개입과 Agent 부재 시 폴백으로 제한한다.

- pause, resume, cancel, retry
- ControlMode를 AGENT, ASSISTED, MANUAL로 전환
- 입력 또는 검수 결과 기록
- 승인 대상과 리비전을 확인한 뒤 Approval 기록
- 같은 Command Bus를 통한 허용된 도메인 기능 실행

버튼과 폼이 실제 Command를 만들고, 채팅은 현재 Job 문맥의 설명·오류 분석·실행 계획 제안에 사용한다. 채팅의 제안은 자동으로 승인으로 간주하지 않는다.

Provider, Worker registry, 인증, Storage와 파괴적 기능 정책은 인증 비활성 모드의 Dashboard에서 바꾸지 않는다. 로컬 설정 파일과 서비스 재시작이 그 책임을 가진다.

## 4. 상태 전달

- 저장된 Event 기반 상태는 SSE reconnect cursor 이후 재생한다. `NTF-001~002`
- 진행률과 자원 metric은 live 상태이며 재연결 때 과거 값을 재생하지 않는다.
- 로그 원문은 파일에 회전·압축하고 Dashboard에는 정제된 tail과 참조만 전달한다.
- 외부 Agent에는 현재 질문에 필요한 최소 문맥만 전달하고 비밀값과 불필요한 원본 경로를 제거한다.

## 5. 초기 범위에서 제외

- 매체 소유권·대여·반환 업무
- 매체 반환 승인과 인수인계 완료 처리
- 원격 관리 설정 편집
- 브라우저에서 임의 shell 또는 임의 파일 작업 실행
- 장기 로그 원문이나 영상 파일을 DB에 저장
