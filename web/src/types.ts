// Protojson names. Image and OccurredAt keep the proto field spelling.
// EmitDefaultValues keeps zero scalars. Unset optional fields stay absent.

export type DateParts = {
  year?: number;
  month?: number;
  day?: number;
};

export type ServiceStatus = {
  status?: "CREATED" | "INTERVIEW" | "READY" | "ERROR" | string;
  error?: string;
  OccurredAt?: string;
};

export type AlgorithmFunction = {
  name?: string;
  parameters?: Array<{
    key?: string;
    defaultValue?: string;
    value?: string;
    valueType?: string;
  }>;
  parallelExecution?: boolean;
  runFirst?: boolean;
  runLast?: boolean;
};

export type Algorithm = {
  filePath?: string;
  functions?: AlgorithmFunction[];
  namespaces?: string[];
};

export type Service = {
  Image?: string;
  algorithm?: Algorithm;
  statuses?: ServiceStatus[];
  ownerLogin?: string;
};

export type BacktestAsset = {
  symbol?: string;
  enabled?: boolean;
};

export type BacktestPeriod = {
  startDate?: DateParts;
  endDate?: DateParts;
  assets?: BacktestAsset[];
};

export type Backtest = {
  name?: string;
  benchmark?: string;
  provider?: string;
  profile?: string;
  periods?: BacktestPeriod[];
  ownerLogin?: string;
};

export type Strategy = {
  id?: string;
  name?: string;
  description?: string;
  createdAt?: string;
  ownerLogin?: string;
};

export type ExecutionResult = {
  date?: DateParts;
  PNL?: number;
  returns?: number;
  portfolioValue?: number;
  endingValue?: number;
  maxDrawdown?: number;
  sharpe?: number;
  // A total return for yahoo and alpaca runs; see Execution.returnBasis.
  algorithmPeriodReturn?: number;
  priceReturn?: number;
  benchmarkPeriodReturn?: number;
  benchmarkPriceReturn?: number;
  positions?: unknown[];
};

// What a run's return measures. UNSPECIFIED is a run stored before
// dividends were counted.
export type ReturnBasis = "RETURN_BASIS_UNSPECIFIED" | "RETURN_BASIS_TOTAL" | "RETURN_BASIS_ADJUSTED_CLOSE";

export type Execution = {
  id?: string;
  backtest?: string;
  session?: string;
  startDate?: DateParts;
  endDate?: DateParts;
  result?: ExecutionResult;
  returnBasis?: ReturnBasis;
  foreignDividendsGross?: number;
};

// GET /api/executions/{id} is GetExecutionResponse, not the execution alone.
export type GetExecution = {
  execution?: Execution;
  periods?: ExecutionResult[];
};
