
package v1beta1

import (
	codec1978 "github.com/ugorji/go/codec"
	"os"
	"reflect"
	"bytes"
	"strings"
	"go/format"
)

func CodecGenTempWrite4327() {
	fout, err := os.Create("ingress.generated.go")
	if err != nil {
		panic(err)
	}
	defer fout.Close()
	var out bytes.Buffer
	
	var typs []reflect.Type 

	var t0 Ingress
	typs = append(typs, reflect.TypeOf(t0))

	var t1 IngressList
	typs = append(typs, reflect.TypeOf(t1))

	var t2 IngressSpec
	typs = append(typs, reflect.TypeOf(t2))

	var t3 IngressTLS
	typs = append(typs, reflect.TypeOf(t3))

	var t4 IngressStatus
	typs = append(typs, reflect.TypeOf(t4))

	var t5 IngressRule
	typs = append(typs, reflect.TypeOf(t5))

	var t6 IngressRuleValue
	typs = append(typs, reflect.TypeOf(t6))

	var t7 HTTPIngressRuleValue
	typs = append(typs, reflect.TypeOf(t7))

	var t8 TCPIngressRuleValue
	typs = append(typs, reflect.TypeOf(t8))

	var t9 HTTPIngressPath
	typs = append(typs, reflect.TypeOf(t9))

	var t10 IngressBackend
	typs = append(typs, reflect.TypeOf(t10))

	var t11 HTTPIngressBackend
	typs = append(typs, reflect.TypeOf(t11))

	var t12 IngressRef
	typs = append(typs, reflect.TypeOf(t12))

	var t13 FrontendRule
	typs = append(typs, reflect.TypeOf(t13))

	codec1978.Gen(&out, "", "v1beta1", "4327", false, codec1978.NewTypeInfos(strings.Split("codec,json", ",")), typs...)
	bout, err := format.Source(out.Bytes())
	if err != nil {
		fout.Write(out.Bytes())
		panic(err)
	}
	fout.Write(bout)
}

