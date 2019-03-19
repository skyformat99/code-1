package agents

import (
	"errors"
	"fmt"
	"github.com/TeaWeb/code/teaconfigs"
	"github.com/TeaWeb/code/teaconfigs/notices"
	"github.com/TeaWeb/code/teautils"
	"github.com/iwind/TeaGo/lists"
	"github.com/iwind/TeaGo/logs"
	"github.com/iwind/TeaGo/types"
	"github.com/iwind/TeaGo/utils/string"
	"github.com/robertkrimen/otto"
	"regexp"
	"strings"
)

// 参数值正则
var thresholdRegexpParamNamedVariable = regexp.MustCompile("\\${[$\\w.-]+}")

// 阈值定义
type Threshold struct {
	Id            string                   `yaml:"id" json:"id"`                       // ID
	Param         string                   `yaml:"param" json:"param"`                 // 参数
	Operator      ThresholdOperator        `yaml:"operator" json:"operator"`           // 运算符
	Value         string                   `yaml:"value" json:"value"`                 // 对比值
	NoticeLevel   notices.NoticeLevel      `yaml:"noticeLevel" json:"noticeLevel"`     // 通知级别
	NoticeMessage string                   `yaml:"noticeMessage" json:"noticeMessage"` // 通知消息
	Actions       []map[string]interface{} `yaml:"actions" json:"actions"`             // 动作配置
	MaxFails      int                      `yaml:"maxFails" json:"maxFails"`           // 连续失败次数

	regValue   *regexp.Regexp
	floatValue float64

	shouldLoop bool   // 是否应该循环测试，如果包含名为$（dollar符号）的变量，则表示是循环测试
	loopVar    string // 要循环的变量
}

// 新阈值对象
func NewThreshold() *Threshold {
	return &Threshold{
		Id: stringutil.Rand(16),
	}
}

// 校验
func (this *Threshold) Validate() error {
	if this.Operator == ThresholdOperatorRegexp || this.Operator == ThresholdOperatorNotRegexp {
		reg, err := regexp.Compile(this.Value)
		if err != nil {
			return err
		}
		this.regValue = reg
	} else if this.Operator == ThresholdOperatorGt || this.Operator == ThresholdOperatorGte || this.Operator == ThresholdOperatorLt || this.Operator == ThresholdOperatorLte {
		this.floatValue = types.Float64(this.Value)
	}

	// 检查参数值
	for _, v := range thresholdRegexpParamNamedVariable.FindAllStringSubmatch(this.Param, -1) {
		varName := v[0][2 : len(v[0])-1]
		pieces := strings.Split(varName, ".")
		if lists.Contains(pieces, "$") {
			this.shouldLoop = true
			this.loopVar = varName
			break
		}
	}

	return nil
}

// 将此条件应用于阈值，检查是否匹配
func (this *Threshold) Test(value interface{}, oldValue interface{}) (ok bool, err error) {
	return this.testParam(this.Param, this.shouldLoop, value, oldValue)
}

// 检查阈值，但指定更多的参数
func (this *Threshold) testParam(param string, shouldLoop bool, value interface{}, oldValue interface{}) (ok bool, err error) {
	// 处理$（dollar符号）
	if shouldLoop {
		pieces := strings.Split(this.loopVar, ".")
		dollarIndex := 0
		for index, piece := range pieces {
			if piece == "$" {
				dollarIndex = index
				break
			}
		}

		if dollarIndex == 0 {
			if types.IsSlice(value) {
				lists.Each(value, func(k int, v interface{}) {
					indexParam := fmt.Sprintf("%d", k)
					if len(pieces) > 1 {
						indexParam += "." + strings.Join(pieces[dollarIndex+1:], ".")
					}
					newParam := strings.Replace(param, "${"+this.loopVar+"}", "${"+indexParam+"}", -1)
					ok1, err1 := this.testParam(newParam, false, value, oldValue)
					if ok1 {
						ok = ok1
						err = err1
					}
				})
				return
			}
		} else {
			newValue := teautils.Get(value, pieces[:dollarIndex])
			if types.IsSlice(newValue) {
				lists.Each(newValue, func(k int, v interface{}) {
					indexParam := strings.Join(pieces[:dollarIndex], ".") + "." + fmt.Sprintf("%d.", k) + strings.Join(pieces[dollarIndex+1:], ".")

					newParam := strings.Replace(param, "${"+this.loopVar+"}", "${"+indexParam+"}", -1)
					ok1, err1 := this.testParam(newParam, false, value, oldValue)
					if ok1 {
						ok = ok1
						err = err1
					}
				})
				return
			}
		}

		return false, nil
	}

	paramValue, err := this.EvalParam(param, value, oldValue)
	if err != nil {
		return false, err
	}

	switch this.Operator {
	case ThresholdOperatorRegexp:
		if this.regValue == nil {
			return false, nil
		}
		return this.regValue.MatchString(types.String(paramValue)), nil
	case ThresholdOperatorNotRegexp:
		if this.regValue == nil {
			return false, nil
		}
		return !this.regValue.MatchString(types.String(paramValue)), nil
	case ThresholdOperatorGt:
		return types.Float64(paramValue) > this.floatValue, nil
	case ThresholdOperatorGte:
		return types.Float64(paramValue) >= this.floatValue, nil
	case ThresholdOperatorLt:
		return types.Float64(paramValue) < this.floatValue, nil
	case ThresholdOperatorLte:
		return types.Float64(paramValue) <= this.floatValue, nil
	case ThresholdOperatorEq:
		return paramValue == this.Value, nil
	case ThresholdOperatorNot:
		return paramValue != this.Value, nil
	case ThresholdOperatorPrefix:
		return strings.HasPrefix(types.String(paramValue), this.Value), nil
	case ThresholdOperatorSuffix:
		return strings.HasSuffix(types.String(paramValue), this.Value), nil
	case ThresholdOperatorContains:
		return strings.Contains(types.String(paramValue), this.Value), nil
	case ThresholdOperatorNotContains:
		return !strings.Contains(types.String(paramValue), this.Value), nil
	}
	return false, nil
}

// 执行数值运算，使用Javascript语法
func (this *Threshold) Eval(value interface{}, old interface{}) (string, error) {
	return this.EvalParam(this.Param, value, old)
}

// 使用某个参数执行数值运算，使用Javascript语法
func (this *Threshold) EvalParam(param string, value interface{}, old interface{}) (string, error) {
	if old == nil {
		old = value
	}
	var resultErr error = nil
	paramValue := teaconfigs.RegexpNamedVariable.ReplaceAllStringFunc(param, func(s string) string {
		if value == nil {
			return ""
		}

		varName := s[2 : len(s)-1]

		// 支持${OLD}和${OLD.xxx}
		if varName == "OLD" {
			result, err := this.EvalParam("${0}", old, nil)
			if err != nil {
				resultErr = err
			}
			return result
		} else if strings.HasPrefix(varName, "OLD.") {
			result, err := this.EvalParam("${"+varName[4:]+"}", old, nil)
			if err != nil {
				resultErr = err
			}
			return result
		}

		switch v := value.(type) {
		case string:
			if varName == "0" {
				return v
			}
			return ""
		case int8, int16, int, int32, int64, uint8, uint16, uint, uint32, uint64:
			if varName == "0" {
				return fmt.Sprintf("%d", v)
			}
			return "0"
		case float32, float64:
			if varName == "0" {
				return fmt.Sprintf("%f", v)
			}
			return "0"
		case bool:
			if varName == "0" {
				if v {
					return "1"
				}
				return "0"
			}
			return "0"
		default:
			if types.IsSlice(value) || types.IsMap(value) {
				result := teautils.Get(v, strings.Split(varName, "."))
				if result == nil {
					return ""
				}
				return types.String(result)
			}
		}
		return s
	})

	// 支持加、减、乘、除、余
	if len(paramValue) > 0 {
		if strings.ContainsAny(paramValue, "+-*/%") {
			vm := otto.New()
			v, err := vm.Run(paramValue)
			if err != nil {
				return "", errors.New("\"" + this.Expression() + "\": eval \"" + paramValue + "\":" + err.Error())
			} else {
				paramValue = v.String()
			}
		}

		// javascript
		if strings.HasPrefix(paramValue, "javascript:") {
			vm := otto.New()
			v, err := vm.Run(paramValue[len("javascript:"):])
			if err != nil {
				return "", errors.New("\"" + this.Expression() + "\": eval \"" + paramValue + "\":" + err.Error())
			} else {
				paramValue = v.String()
			}
		}
	}

	return paramValue, resultErr
}

// 执行动作
func (this *Threshold) RunActions(params map[string]string) error {
	if len(this.Actions) == 0 {
		return nil
	}

	for _, a := range this.Actions {
		code, found := a["code"]
		if !found {
			return errors.New("action 'code' not found")
		}

		options, found := a["options"]
		if !found {
			return errors.New("action 'options' not found")
		}
		optionsMap, ok := options.(map[string]interface{})
		if !ok {
			return errors.New("action 'options' should be a valid map")
		}

		action := FindAction(types.String(code))
		if action == nil {
			return errors.New("action for '" + types.String(code) + "' not found")
		}

		instance := action["instance"]
		err := teautils.MapToObjectJSON(optionsMap, &instance)
		if err != nil {
			return err
		}

		output, err := instance.(ActionInterface).Run(params)
		if err != nil {
			return err
		}
		if len(output) > 0 {
			logs.Println("[threshold]run actions:", output)
		}
	}

	return nil
}

// 取得描述文本
func (this *Threshold) Expression() string {
	return this.Param + " " + this.Operator + " " + this.Value
}
