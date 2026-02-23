# BOB - Black Ouroborotic Box

BOB is a project aiming to be fully autonomous code developing system.

BOB has following main components:
- bob - command line tool that starts main loop and manages the system
    - orchestrator - part of bob process that manages agent instances and their interactions
    - mcp-api - part of bob process API that agents can use to request bob to communicate with other agents

Agents:
- solver - is responsible to write a solution - code that solves the problem
- evaluator - is responsible to write tests that evaluate the solution and run them to provide feedback to solver
- architect - is responsible to receive specification of the problem and write and maintain technical contract that explains what evaluator can expect solution to do
- (optional) reviewer - is responsible to review solution code and provide feedback to solver

Sources:
Every agent should have its own git repository. Agents expected to commit changes to their files. Agent instance instance when started would always receive fresh clone of their repository.

Limitations:
- solver MUST NOT see evaluator's repository of tests to avoid overfitting solution to tests
- evaluator MUST NOT see solver's repository of solution to avoid overfitting tests to the solution, evaluator only sees original spec and technical contract
- reviewer MUST NOT see architect's repository with specs, but can see solution and test repositories and be able to provide feedback on their code quality (ONLY)
- architect MUST NOT see any other repositories
- neither of agents should know that they are orchestrated by bob nor arhitecture of bob, they should only get context important for their work

## Repository structures

### Solver repositories structure

```.
solution/ # contains code of the solution, solver is started on working directory of this repository
├── .claude/ # contains context needed for solver to do its work
├── .gitignore # ignore at least "architecture" and "solution-deliverable" folders
├── architecture/ # content of architect repository, would be ignored by git, but available to solver for reading
├── solution-deliverable/ # result of solution delivery, would be ignored by git, but available to solver for reading, contains results of solution build (usually not source code, except if solution is a library)
```

```.
evaluation/ # contains code of the evaluation, evaluator is started on working directory of this repository
├── .claude/ # contains context needed for evaluator to do its work
├── .gitignore # ignore at least "architecture" and "solution-deliverable" folders
├── architecture/ # content of architect repository, would be ignored by git, but available to evaluator for reading
├── solution-deliverable/ # result of solution delivery, would be ignored by git, but available to evaluator for reading, contains results of solution build (usually not source code, except if solution is a library)
├── <...> # ANY other files that evaluator needs
```

```.
architecture/ # contains content of architect repository, architect is started on working directory of this repository
├── .claude/ # contains context needed for architect to do its work
├── SPECIFICATION.md # specification of the problem, written by architect (possibly with help of human)
├── <...> # ANY other files that architect needs to define the technical contract
```

```
review/ # contains code of the reviewer, reviewer is started on working directory of this repository
├── .claude/ # contains context needed for reviewer to do its work
├── .gitignore # ignore at least "architecture" folder
├── architecture/ # content of architect repository, would be ignored by git, but available to reviewer for reading
├── solution/ # content of solver repository, would be ignored by git, but available to reviewer for reading
```

### Bob commands

`bob init` - creates its own ".bob" folder, then four folders: solution, evaluation, architecture and review; Initializes git repositories in each of them and creates .claude folders (and .gitignore and everything else required) in each of them with initial context for agents to do their work
`bob up` - starts the main loop of bob, which defines which agents are to be started
`bob mcp` - starts an MCP server with stdio transport that would allow agents to use MCP to perform controlled communication between each other

### Bob technical details

Bob should be implemented in Golang.
Bob should start agent instances as separate processes, at the moment only implementing start of "claude" with flags `--verbose` and `--output-format=stream-json`, other possible agents might be added later.
Bob should provide to agents context in .claude folder in their repositories, that would contain at least:
- settings.json - file describing what agent is allowed to do, all tools should be explicitly allowed or denied here, this is important so that agents do not wait for user input
- CLAUDE.md - file describing what agent is supposed to be doing in general terms, MUST NOT specify details of bob architecture, i.e for solver only explains minimum needed context - folder structure and task
Bob should initialize repositories with starting ".gitignore" too

Bob MCP should include these tools at least:
- when run as `bob mcp --agent solver`: 
  - handoff_solution # bob copies solution deliverable from solver repository to evaluation repository, then start evaluator instance, bob also copies solution code to reviewer repository and instantiate reviewer
  - request_architecture_change # bob takes arbitrary request and instantiates architect instance with this request passed, after architect changes the architecture, it is copied to solver and evaluator
- when run as `bob mcp --agent evaluator`:
  - request_architecture_change # bob takes arbitrary request and instantiates architect instance with this request passed, after architect changes the architecture, it is copied to solver and evaluator
  - request_solution_change # bob takes arbitrary request and instantiates solver instance with this request passed
  - confirm_solution # bob takes solution deliverable and copies it to output
- when run as `bob mcp --agent reviewer`:
  - request_solution_change # bob takes arbitrary request and instantiates solver instance with this request passed, after solver changes the solution, it is copied to evaluator and reviewer

Important design note: when written above "bob instantiates" a particular agent, this means that bob puts this request to a agent-scoped queue (locally run) and only starts agent instance when no agents of that same type are currently run, i.e no parallel instances of the same agent are run at the same time.

Queuing:
- bob manages queues as files under ".bob/queues" folder
- queues are folders with request-<timestamp>.yaml files structured appoximately like this:

```yaml
timestamp: 2024-06-01T12:00:00Z # RFC3339 timestamp of when the request was created, used to process requests in order
request:
  type: request_solution_change # type of request
  from: evaluator # who made the request, mostly used as logging info
  additional_context: <...> # any additional context needed to process the request, for example commit hash of the solution that is to be evaluated
```

### Bob loop

When bob primary orchestrator starts, it needs to check running queues by reading files under ".bob/queues" folder, then run agents according to the requests.
Technically agents (i.e "claude") each start their own bob mcp server in their own folders, bob mcp server writing queues should work on "../.bob/queues" folder.
When new request should enter queue, bob writes new file to "../.bob/queues" folder, then primary orchestrator sees the new file, checks the type of request and enqueues it internally (probably in channel), then when request is scheduled internally instantiates an agent appropriately. When agent finishes work, corresponding in-flight request is removed from the queue (deleted from ".bob/queues" folder, but then written in ".bob/completed" folder).
