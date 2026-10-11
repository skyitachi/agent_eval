# 目标
1. 设计实现一个能够评估agent harness，大模型，skill各自的能力的评测框架，最终产出是一个命令行工具
2. 评测数据能够从现有的agent会话中收集（用已有会话中模型的tools调用和最终输出作为baseline)，也能支持用户手动构建评测数据集
3. 评测标准至少包括, 完成任务的正确性，消耗的token和完成任务的时间

# 实现要求
1. 当前可以仅支持基于claude code agent sdk（agent harness固定为claude code），但时候要设计成可以支持任意agent
2. 收集agent的已有会话，要支持收集tools的调用，用户的回复等整个轨迹
